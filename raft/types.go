// Package raft types: domain types used throughout the consensus engine.
//
// Wire types live in raft/raftpb (proto-generated); (de)serialization to
// those happens at the RPC boundary in election.go and replication.go.
// Keeping our own LogEntry avoids embedding protoimpl.MessageState (which
// contains a sync.Mutex) so we can range over []LogEntry cleanly.

package raft

// LogEntry is one entry in the replicated log. LogBase + array arithmetic
// (log[absIndex - logBase - 1]) is documented in AGENTS.md; for 0.1.0
// logBase is always 0.
type LogEntry struct {
	Index   int64
	Term    int64
	Command []byte // opaque payload; kv.Command serialized by callers
}

// State is the Raft role of a node.
type State int

const (
	Follower State = iota
	Candidate
	Leader
)

// String returns a human-readable name for the state. Useful in slog.
func (s State) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// ApplyMsg is sent to the application (KV service) when a log entry is
// committed. Snapshot fields are zero-valued and unused in 0.1.0; the
// type must not change when snapshotting lands (see AGENTS.md
// "Snapshotting (deferred)").
type ApplyMsg struct {
	CommandValid bool
	Command      []byte
	CommandIndex int

	SnapshotValid     bool
	SnapshotData      []byte
	LastIncludedIndex int
	LastIncludedTerm  int
}
