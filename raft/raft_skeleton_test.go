package raft_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConfig builds a minimal Config for tests: 3-node setup with
// ephemeral-looking addresses, MemoryPersister, deterministic timeouts.
// Caller can mutate the returned Config before passing to NewRaft.
func newTestConfig() raft.Config {
	return raft.Config{
		ServerID: 1,
		OwnAddr:  "127.0.0.1:10001",
		Peers: map[int]string{
			2: "127.0.0.1:10002",
			3: "127.0.0.1:10003",
		},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		SnapshotThreshold:  0,
		Persister:          raft.NewMemoryPersister(),
		Logger:             slog.Default(),
	}
}

// TestNewRaft_InitialState verifies a fresh node starts with the canonical
// "brand new follower" state: term 0, votedFor -1, leaderId -1 (unknown),
// empty log, commitIndex/lastApplied at 0, logBase at 0, Follower state.
func TestNewRaft_InitialState(t *testing.T) {
	rf, err := raft.NewRaft(newTestConfig())
	require.NoError(t, err)

	assert.Equal(t, raft.Follower, rf.State(), "fresh node must be Follower")
	assert.Equal(t, 0, rf.CurrentTerm(), "fresh node term is 0")
	assert.Equal(t, -1, rf.VotedFor(), "fresh node votedFor is -1")
	assert.Equal(t, -1, rf.GetLeaderId(), "leaderId unknown on startup")
	assert.Equal(t, 0, rf.CommitIndex(), "commitIndex starts at 0")
	assert.Equal(t, 0, rf.LastApplied(), "lastApplied starts at 0")
	assert.Equal(t, 0, rf.LogBase(), "logBase is 0 in 0.1.0")
	assert.Empty(t, rf.Log(), "log is empty on fresh node")
}

// TestNewRaft_Accessors checks the trivial getters.
func TestNewRaft_Accessors(t *testing.T) {
	rf, err := raft.NewRaft(newTestConfig())
	require.NoError(t, err)

	assert.Equal(t, 1, rf.GetServerId())
	assert.Equal(t, "127.0.0.1:10001", rf.GetAddress())
	assert.Equal(t, 2, rf.GetPeerCount(), "peers are 2 and 3")
	assert.False(t, rf.IsLeader(), "fresh node is not leader")
	assert.NotNil(t, rf.GetApplyCh(), "applyCh must be allocated")
}

// TestNewRaft_PeersExcludesSelf asserts the peer map does not contain
// the node's own id (a common 0.0.x bug class).
func TestNewRaft_PeersExcludesSelf(t *testing.T) {
	cfg := newTestConfig()
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)

	// GetPeerCount already hints; spelling it out:
	assert.Equal(t, len(cfg.Peers), rf.GetPeerCount())
}

// TestNewRaft_LoadsPersistedState verifies the constructor restores
// currentTerm, votedFor, and log from the persister before Start.
func TestNewRaft_LoadsPersistedState(t *testing.T) {
	p := raft.NewMemoryPersister()
	// Pre-populate as if a previous incarnation had saved.
	require.NoError(t, p.Save(7, 3, []raft.LogEntry{
		{Index: 1, Term: 5, Command: []byte("a")},
		{Index: 2, Term: 7, Command: []byte("b")},
	}))

	cfg := newTestConfig()
	cfg.Persister = p
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)

	assert.Equal(t, 7, rf.CurrentTerm(), "term must be restored from persister")
	assert.Equal(t, 3, rf.VotedFor(), "votedFor must be restored")
	require.Len(t, rf.Log(), 2, "log must be restored")
	assert.Equal(t, int64(2), rf.Log()[1].Index)
}

// TestNewRaft_LoadsPersistedState_AdvancesCommitApplied asserts that
// when persisted log is non-empty, commitIndex and lastApplied are
// snapped to the last log entry's index so we don't replay already-
// applied entries. (See AGENTS.md "Recovery" for snapshot case; same
// principle for 0.1.0 non-snapshot recovery.)
func TestNewRaft_LoadsPersistedState_AdvancesCommitApplied(t *testing.T) {
	p := raft.NewMemoryPersister()
	require.NoError(t, p.Save(5, 2, []raft.LogEntry{
		{Index: 1, Term: 1, Command: []byte("x")},
		{Index: 2, Term: 3, Command: []byte("y")},
		{Index: 3, Term: 5, Command: []byte("z")},
	}))

	cfg := newTestConfig()
	cfg.Persister = p
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)

	assert.Equal(t, 3, rf.CommitIndex(), "commitIndex snaps to last log index")
	assert.Equal(t, 3, rf.LastApplied(), "lastApplied snaps to last log index")
}

// TestNewRaft_EmptyPeersSingleNode asserts a single-node config (no peers)
// constructs cleanly. Single-node self-election is exercised in 3c tests.
func TestNewRaft_EmptyPeersSingleNode(t *testing.T) {
	cfg := newTestConfig()
	cfg.ServerID = 0
	cfg.OwnAddr = "127.0.0.1:9999"
	cfg.Peers = map[int]string{}
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)
	assert.Equal(t, 0, rf.GetPeerCount())
}

// TestNewRaft_MissingPersisterErrors validates the constructor surfaces
// a clear error when the persister is nil rather than panicking later.
func TestNewRaft_MissingPersisterErrors(t *testing.T) {
	cfg := newTestConfig()
	cfg.Persister = nil
	_, err := raft.NewRaft(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persister")
}

// TestNewRaft_NilLoggerDefaults verifies a nil logger falls back to
// slog.Default() rather than panicking on first log call.
func TestNewRaft_NilLoggerDefaults(t *testing.T) {
	cfg := newTestConfig()
	cfg.Logger = nil
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)
	assert.NotNil(t, rf.Logger(), "logger should default to slog.Default()")
}

// TestNewRaft_PeerMapIncludesSelfErrors asserts the constructor rejects
// a peer map that includes the node's own id (a configuration mistake).
func TestNewRaft_PeerMapIncludesSelfErrors(t *testing.T) {
	cfg := newTestConfig()
	cfg.Peers[1] = "127.0.0.1:10001" // self in peers
	_, err := raft.NewRaft(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "self")
}

// TestNewRaft_InvalidTimeoutsErrors covers the election-timeout sanity
// checks so misconfigurations fail loudly rather than producing weird
// runtime behavior.
func TestNewRaft_InvalidTimeoutsErrors(t *testing.T) {
	tests := []struct {
		name string
		mut  func(c *raft.Config)
		want string
	}{
		{"min zero", func(c *raft.Config) { c.ElectionTimeoutMin = 0 }, "election_timeout_min"},
		{"max zero", func(c *raft.Config) { c.ElectionTimeoutMax = 0 }, "election_timeout_max"},
		{"min >= max", func(c *raft.Config) {
			c.ElectionTimeoutMin = 300 * time.Millisecond
			c.ElectionTimeoutMax = 150 * time.Millisecond
		}, "less than"},
		{"heartbeat zero", func(c *raft.Config) { c.HeartbeatInterval = 0 }, "heartbeat_interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newTestConfig()
			tt.mut(&cfg)
			_, err := raft.NewRaft(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// TestNewRaft_DoesNotStartGoroutines verifies the constructor's lifecycle
// invariant: NewRaft must not start the election timer, apply loop, or
// heartbeat sender. Start() does. We check by asserting commitIndex/term
// stay at their initial values for a short interval after construction.
func TestNewRaft_DoesNotStartGoroutines(t *testing.T) {
	rf, err := raft.NewRaft(newTestConfig())
	require.NoError(t, err)

	// With no goroutines running and no Start() called, the node's state
	// cannot change. Wait a little longer than the election timeout max
	// and assert we're still a fresh follower.
	time.Sleep(350 * time.Millisecond)

	assert.Equal(t, raft.Follower, rf.State(), "no election should run without Start()")
	assert.Equal(t, 0, rf.CurrentTerm(), "term must not advance without Start()")
	assert.False(t, rf.IsLeader())
}

// TestNewRaft_SnapshotThresholdStored verifies the config knob is passed
// through (parsed but unused in 0.1.0 per AGENTS.md snapshotting notes).
func TestNewRaft_SnapshotThresholdStored(t *testing.T) {
	cfg := newTestConfig()
	cfg.SnapshotThreshold = 500
	rf, err := raft.NewRaft(cfg)
	require.NoError(t, err)
	assert.Equal(t, 500, rf.SnapshotThreshold())
}
