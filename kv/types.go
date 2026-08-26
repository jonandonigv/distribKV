// Package kv implements the key-value state machine that sits on top of
// the raft consensus engine. It exposes Get/Put/Append RPCs to clients
// and applies committed raft log entries to an in-memory map.
//
// Design (see AGENTS.md "KV Service Notes"):
//   - Every operation including Get goes through the raft log (provably
//     linearizable, simple). ReadIndex is a later optimization.
//   - Duplicate detection via clientId/seqNum cache (cap 100/client,
//     TTL 10s). Dedup cache is NOT persisted — documented gap.
//   - clientId is 8-byte crypto/rand int64 (never time.Now().UnixNano()).
//   - Wrong-leader responses carry {wrong_leader, leader_id}.
//
// Proto-generated types (Command, OpType, GetRequest/Response, etc.)
// live in this same package (kv.pb.go, kv_grpc.pb.go). We use them
// directly: Command is the op type serialized into the raft log.
package kv

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonandonigv/distribKV/raft"
	"google.golang.org/grpc"
)

// Errors returned by KVServer RPC handlers.
var (
	ErrNotLeader      = errors.New("not leader")
	ErrTimeout        = errors.New("timeout waiting for commit")
	ErrKeyNotFound    = errors.New("key not found")
	ErrDuplicate      = errors.New("duplicate request")
	ErrTooManyPending = errors.New("too many pending operations")
	ErrShutdown       = errors.New("server shutting down")
)

// Constants for KVServer tuning (see AGENTS.md "KV Service Notes").
const (
	DefaultMaxPendingOps         = 1000
	RPCTimeout                   = 5 * time.Second
	maxDuplicateEntriesPerClient = 100
	duplicateCacheExpiry         = 10 * time.Second
)

// Op is the internal representation of a client operation. It's a plain
// struct (no protoimpl.MessageState mutex) so we can pass it by value
// without tripping go vet. Converted to/from proto Command only at the
// raft log serialization boundary in serializeCommand/deserializeCommand.
type Op struct {
	Type       OpKind
	Key        string
	Value      string
	ClientId   int64
	SequenceId int64
}

// OpKind mirrors the proto enum but is our own domain type.
type OpKind int

const (
	OpGet OpKind = iota + 1
	OpPut
	OpAppend
)

// toProto converts a domain Op to a proto Command for serialization.
func (o Op) toProto() Command {
	return Command{
		Op:          OpType(o.Type),
		Key:         o.Key,
		Value:       o.Value,
		ClientId:    o.ClientId,
		SequenceNum: o.SequenceId,
	}
}

// opFromProto converts a proto Command back to a domain Op. Takes a
// pointer to avoid copying the proto-generated struct (which embeds
// protoimpl.MessageState containing a sync.Mutex).
func opFromProto(cmd *Command) Op {
	return Op{
		Type:       OpKind(cmd.Op),
		Key:        cmd.Key,
		Value:      cmd.Value,
		ClientId:   cmd.ClientId,
		SequenceId: cmd.SequenceNum,
	}
}

// Result is what the apply loop sends back to the RPC handler that
// submitted the op.
type Result struct {
	Value string
	Err   error
}

// PendingOp tracks an in-flight RPC waiting for raft to commit its entry.
type PendingOp struct {
	Index    int
	Op       Op
	ResultCh chan Result
}

// DuplicateEntry caches the result of a previously-applied op so a
// client retry returns the same answer without re-executing.
type DuplicateEntry struct {
	Result    Result
	Timestamp time.Time
}

// Server is the state-machine layer. It reads committed entries from
// raft's applyCh, applies them to an in-memory map, and notifies the
// RPC handler waiting on each entry's index. Construct with NewServer;
// stop with Kill.
type Server struct {
	UnimplementedKVServer // embed for forward compat

	mu         sync.Mutex
	rf         *raft.Raft
	applyCh    chan raft.ApplyMsg
	state      map[string]string
	duplicates map[int64]map[int64]*DuplicateEntry // clientId -> seqNum -> entry
	pendingOps map[int]*PendingOp                  // logIndex -> waiter
	recent     map[int]Result                      // logIndex -> result (no waiter yet)
	maxPending int
	leaderId   int
	dead       bool
	shutdownCh chan struct{}

	// Snapshot bookkeeping (PLAN.md Step S1d). lastApplied tracks every
	// applied index (commands AND snapshot rehydration); lastSnapshotIndex
	// is what we last folded into rf.Snapshot. The trigger fires when
	// lastApplied - lastSnapshotIndex >= rf.SnapshotThreshold().
	lastApplied       int
	lastSnapshotIndex int

	logger *slog.Logger
}

// Clerk is the client library for talking to a distribKV cluster.
// It retries against any node, caches the leader, and deduplicates
// via monotonically-increasing sequence numbers. See clerk.go.
type Clerk struct {
	servers   []string
	serverIds []int
	leaderId  int
	clientId  int64
	seqNum    atomic.Int64
	mu        sync.Mutex
	verbose   bool

	// gRPC connection cache; lazily dialed on first use.
	conns map[int]*grpc.ClientConn
}

// ---------------------------------------------------------------------------
// Serialization helpers — Command <-> []byte for the raft log.
// ---------------------------------------------------------------------------

// serializeCommand converts an Op to bytes for the raft log. We marshal
// the proto Command via JSON; the raft layer treats Command.Command as
// opaque bytes.
func serializeCommand(op Op) ([]byte, error) {
	return json.Marshal(op.toProto())
}

// deserializeCommand converts raft log bytes back to an Op. Returns an
// error on malformed input rather than panicking (closes 0.0.x TODO #28).
func deserializeCommand(data []byte) (Op, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return Op{}, fmt.Errorf("deserialize command: %w", err)
	}
	return opFromProto(&cmd), nil
}

// ---------------------------------------------------------------------------
// ClientId generation — 8-byte crypto/rand int64.
// ---------------------------------------------------------------------------

// newClientID generates a random 64-bit client identifier using
// crypto/rand (never time.Now().UnixNano() — see 0.0.x TODO #32).
func newClientID() int64 {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	var id int64
	for _, b := range buf {
		id = (id << 8) | int64(b)
	}
	return id
}

// newSequenceNum returns the next sequence number for a Clerk.
// Thread-safe via atomic.Int64.
func (ck *Clerk) newSequenceNum() int64 {
	return ck.seqNum.Add(1)
}

// rpcCtx returns a context with the standard RPC timeout.
func rpcCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), RPCTimeout)
}
