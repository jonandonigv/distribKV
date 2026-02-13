package kvserver

import (
	"errors"
	"sync"
	"time"

	"github.com/jonandonigv/distribKV/pkg/raft"
	pb "github.com/jonandonigv/distribKV/proto/kv"
	"google.golang.org/grpc"
)

var (
	ErrNotLeader   = errors.New("not leader")
	ErrTimeout     = errors.New("timeout waiting for commit")
	ErrKeyNotFound = errors.New("key not found")
	ErrDuplicate   = errors.New("duplicate operation")
)

type OpType int

const (
	OpGet OpType = iota
	OpPut
	OpAppend
)

type Op struct {
	Type       OpType
	Key        string
	Value      string
	ClientId   int64
	SequenceId int64
}

type Result struct {
	Value string
	Err   error
}

type PendingOp struct {
	Index    int
	Op       Op
	ResultCh chan Result
}

// DuplicateEntry tracks completed operations with timestamp for cache eviction
type DuplicateEntry struct {
	Result    Result
	Timestamp time.Time
}

type KVServer struct {
	mu         sync.Mutex
	rf         *raft.Raft
	applyCh    chan raft.ApplyMsg
	state      map[string]string
	duplicates map[int64]map[int64]*DuplicateEntry
	pendingOps map[int]*PendingOp
	leaderId   int
	dead       bool
	shutdownCh chan struct{}
}

type Clerk struct {
	servers  []string
	leaderId int
	clientId int64
	seqNum   int64
	mu       sync.Mutex
	conns    map[int]*grpc.ClientConn
	clients  map[int]pb.KVClient
}
