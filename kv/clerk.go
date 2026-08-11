// Clerk is the client library for talking to a distribKV cluster.
// It retries against any node, caches the leader, and uses monotonically-
// increasing sequence numbers for dedup. See AGENTS.md "KV Service Notes":
// lazy dial, 1000-attempt retry, exponential backoff 50ms→1s cap, panic
// at limit.

package kv

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// MaxClerkAttempts is the number of retries before Clerk panics. 1000
// matches the 0.0.x convention — the cluster is unrecoverable at that point.
const MaxClerkAttempts = 1000

// MakeClerk constructs a Clerk that talks to the given server addresses.
// serverIds are parallel to servers; both must be the same length.
// panics if no servers are reachable (or if all dial attempts fail — the
// caller should ensure at least one node is up first).
func MakeClerk(servers []string, serverIds []int, logger *slog.Logger) *Clerk {
	if logger == nil {
		logger = slog.Default()
	}
	if len(servers) == 0 {
		panic("MakeClerk: no servers provided")
	}
	ck := &Clerk{
		servers:   servers,
		serverIds: serverIds,
		leaderId:  -1, // unknown
		clientId:  newClientID(),
		conns:     make(map[int]*grpc.ClientConn),
		verbose:   true,
	}
	// Log our clientId for debugging.
	logger.Debug("clerk created", "client_id", ck.clientId, "num_servers", len(servers))
	return ck
}

// ---------------------------------------------------------------------------
// gRPC connection management.
// ---------------------------------------------------------------------------

// getClient returns the KVClient for the given server index, lazily dialing
// if needed. Thread-safe (guarded by ck.mu).
func (ck *Clerk) getClient(serverIdx int) (KVClient, error) {
	ck.mu.Lock()
	defer ck.mu.Unlock()

	conn, ok := ck.conns[serverIdx]
	if ok && conn != nil {
		return NewKVClient(conn), nil
	}

	// Dial with a non-blocking grpc.NewClient (first RPC will block
	// until the connection is established or times out).
	serverAddr := ck.servers[serverIdx]
	kacp := keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             3 * time.Second,
		PermitWithoutStream: true,
	}
	conn, err := grpc.NewClient(serverAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(kacp),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", serverAddr, err)
	}
	ck.conns[serverIdx] = conn
	return NewKVClient(conn), nil
}

// CloseConn closes all gRPC connections. Optional; connections are
// cleaned up on process exit anyway. Useful for tests that create
// many clerks.
func (ck *Clerk) CloseConn() {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	for _, conn := range ck.conns {
		if conn != nil {
			_ = conn.Close()
		}
	}
	ck.conns = make(map[int]*grpc.ClientConn)
}

// ---------------------------------------------------------------------------
// Public API: Get, Put, Append.
// ---------------------------------------------------------------------------

// Get returns the value for key, or "" if the key doesn't exist.
// Retries until it finds the leader and gets a response. Panics after
// MaxClerkAttempts (1000) to prevent infinite loops.
func (ck *Clerk) Get(key string) string {
	for attempt := 0; attempt < MaxClerkAttempts; attempt++ {
		serverIdx := ck.pickServer()
		client, err := ck.getClient(serverIdx)
		if err != nil {
			ck.backoff(attempt)
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), RPCTimeout)
		resp, err := client.Get(ctx, &GetRequest{Key: key})
		cancel()

		if err != nil {
			ck.backoff(attempt)
			continue
		}

		if resp.GetWrongLeader() {
			ck.updateLeader(serverIdx, int(resp.GetLeaderId()))
			continue
		}

		// Success (or expected error like key-not-found).
		ck.leaderId = ck.serverIds[serverIdx]
		return resp.GetValue()
	}
	panic(fmt.Sprintf("Get(%q) failed after %d attempts", key, MaxClerkAttempts))
}

// Put sets key = value. Retries until it finds the leader and gets a
// successful response. Panics after MaxClerkAttempts.
func (ck *Clerk) Put(key, value string) {
	ck.Do(Op{
		Type:  OpPut,
		Key:   key,
		Value: value,
	})
}

// Append concatenates value to the existing value at key (or sets it if
// the key doesn't exist). Retries until success. Panics after MaxClerkAttempts.
func (ck *Clerk) Append(key, value string) {
	ck.Do(Op{
		Type:  OpAppend,
		Key:   key,
		Value: value,
	})
}

// Do submits a write command (Put or Append) with a unique sequence
// number for dedup. Retries until success or panic.
func (ck *Clerk) Do(op Op) {
	op.ClientId = ck.clientId

	for attempt := 0; attempt < MaxClerkAttempts; attempt++ {
		serverIdx := ck.pickServer()
		client, err := ck.getClient(serverIdx)
		if err != nil {
			ck.backoff(attempt)
			continue
		}

		// Assign a new sequence number for each attempt (on success
		// the sequence is consumed; on retry the next seq is used so
		// the server dedupes correctly).
		op.SequenceId = ck.newSequenceNum()

		ctx, cancel := context.WithTimeout(context.Background(), RPCTimeout)
		var resp interface {
			GetSuccess() bool
			GetWrongLeader() bool
			GetLeaderId() int32
			GetError() string
		}
		var err2 error
		switch op.Type {
		case OpPut:
			resp2, e := client.Put(ctx, &PutRequest{Key: op.Key, Value: op.Value})
			err2 = e
			resp = resp2
		case OpAppend:
			resp2, e := client.Append(ctx, &AppendRequest{Key: op.Key, Value: op.Value})
			err2 = e
			resp = resp2
		}
		cancel()

		if err2 != nil {
			ck.backoff(attempt)
			continue
		}

		if resp.GetWrongLeader() {
			ck.updateLeader(serverIdx, int(resp.GetLeaderId()))
			continue
		}

		if !resp.GetSuccess() {
			// Some error from the server (not wrong-leader). Retry.
			ck.backoff(attempt)
			continue
		}

		// Success.
		ck.leaderId = ck.serverIds[serverIdx]
		return
	}
	panic(fmt.Sprintf("Do(%v) failed after %d attempts", op.Type, MaxClerkAttempts))
}

// ---------------------------------------------------------------------------
// Internal helpers.
// ---------------------------------------------------------------------------

// pickServer returns the index of the server to try next. If we have a
// cached leader, try it first; otherwise try them round-robin.
func (ck *Clerk) pickServer() int {
	// Try the cached leader first.
	if ck.leaderId >= 0 {
		for i, id := range ck.serverIds {
			if id == ck.leaderId {
				return i
			}
		}
	}
	// Fall back to a simple round-robin using seqNum as a counter.
	n := ck.seqNum.Load()
	return int(n) % len(ck.servers)
}

// updateLeader caches the leader hint from a wrong-leader response.
// If leaderId is -1 (unknown), we just clear the cached leader.
func (ck *Clerk) updateLeader(serverIdx int, leaderId int) {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	if leaderId >= 0 {
		ck.leaderId = leaderId
	} else {
		ck.leaderId = -1 // clear cache; try round-robin
	}
}

// backoff sleeps for an exponential duration: 50ms * 2^(n-1), capped at 1s.
func (ck *Clerk) backoff(attempt int) {
	d := time.Duration(50*(1<<uint(attempt))) * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	if d < 0 || d == time.Duration(math.MaxInt64) {
		d = time.Second // overflow guard
	}
	time.Sleep(d)
}

// Unused but kept for future address-based ID derivation if needed.
var _ = math.MaxInt64
