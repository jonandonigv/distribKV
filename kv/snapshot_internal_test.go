// Whitebox tests for KV-side snapshot handling (PLAN.md Step S1d):
// snapshot serialization round-trips, rehydration from SnapshotValid
// ApplyMsgs, and the applied-entry-count trigger feeding rf.Snapshot.
//
// Trigger tests drive REAL commits through the public AppendEntries
// handler (no leader gymnastics needed) so rf.Snapshot's commitIndex
// guard holds naturally.

package kv

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft"
	raftpb "github.com/jonandonigv/distribKV/raft/raftpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// newTestServerWithRaft wraps a pre-built raft node in a unit-test Server
// WITHOUT starting the applyLoop goroutine (callers feed ApplyMsgs
// directly to processApplyMsg).
func newTestServerWithRaft(t *testing.T, rf *raft.Raft) *Server {
	t.Helper()
	return &Server{
		rf:         rf,
		applyCh:    rf.GetApplyCh(),
		state:      make(map[string]string),
		duplicates: make(map[int64]map[int64]*DuplicateEntry),
		pendingOps: make(map[int]*PendingOp),
		recent:     make(map[int]Result),
		maxPending: DefaultMaxPendingOps,
		shutdownCh: make(chan struct{}),
		logger:     slog.New(slog.NewTextHandler(&kvTestLogWriter{t: t}, nil)),
	}
}

func newTestServerWithLogger(t *testing.T) *Server {
	t.Helper()
	rf := newTestRaftForSnapshotWithPersister(t, 0, raft.NewMemoryPersister())
	return newTestServerWithRaft(t, rf)
}

func newTestRaftForSnapshotWithPersister(t *testing.T, threshold int, p raft.Persister) *raft.Raft {
	t.Helper()
	rf, err := raft.NewRaft(raft.Config{
		ServerID:           1,
		OwnAddr:            "127.0.0.1:0",
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		SnapshotThreshold:  threshold,
		Persister:          p,
		Logger:             slog.New(slog.NewTextHandler(&kvTestLogWriter{t: t}, nil)),
	})
	require.NoError(t, err)
	return rf
}

type kvTestLogWriter struct{ t *testing.T }

func (w *kvTestLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// cmdPut serializes a Put op the same way submitOperation does.
func cmdPut(key, value string) []byte {
	data, err := serializeCommand(Op{Type: OpPut, Key: key, Value: value})
	if err != nil {
		panic(err)
	}
	return data
}

// cmdEntry wraps serialized bytes as a wire log entry.
func cmdEntry(index int64, cmd []byte) *raftpb.LogEntry {
	return &raftpb.LogEntry{Index: index, Term: 1, Command: cmd}
}

// aeReqWithoutEntries builds a heartbeat-style AppendEntries request.
func aeReqWithoutEntries(term int, prevIdx int, prevTerm int) *raftpb.AppendEntriesRequest {
	return &raftpb.AppendEntriesRequest{
		Term:         int64(term),
		LeaderId:     2,
		PrevLogIndex: int64(prevIdx),
		PrevLogTerm:  int64(prevTerm),
		LeaderCommit: 0,
	}
}

// aeReqWithEntries builds an AppendEntries request carrying entries and a
// leaderCommit equal to the last entry index (i.e. fully committed).
func aeReqWithEntries(prevIdx int, prevTerm int, entries ...*raftpb.LogEntry) *raftpb.AppendEntriesRequest {
	req := &raftpb.AppendEntriesRequest{
		Term:         1,
		LeaderId:     2,
		PrevLogIndex: int64(prevIdx),
		PrevLogTerm:  int64(prevTerm),
	}
	for _, e := range entries {
		req.Entries = append(req.Entries, e)
		if len(req.Entries) == len(entries) {
			req.LeaderCommit = e.Index
		}
	}
	return req
}

func onlyClient(dedup map[int64]map[int64]*DuplicateEntry) int64 {
	for id := range dedup {
		return id
	}
	return 0
}

// ---------------------------------------------------------------------------
// Serialization round-trip + corrupt input.
// ---------------------------------------------------------------------------

func TestSerializeDeserializeSnapshot_RoundTrip(t *testing.T) {
	state := map[string]string{"a": "1", "b": "22"}
	dedup := map[int64]map[int64]*DuplicateEntry{
		100: {
			1: {Result: Result{Value: "x"}, Timestamp: time.Now()},
			2: {Result: Result{}, Timestamp: time.Now()},
		},
	}

	data, err := serializeSnapshot(state, dedup)
	require.NoError(t, err)

	gotState, gotDedup, err := deserializeSnapshot(data)
	require.NoError(t, err)
	assert.Equal(t, state, gotState)

	require.Len(t, gotDedup, 1)
	require.Len(t, gotDedup[100], 2)
	assert.Equal(t, "x", gotDedup[100][1].Result.Value)
	assert.WithinDuration(t, dedup[100][1].Timestamp, gotDedup[100][1].Timestamp, time.Second)
}

func TestDeserializeSnapshot_CorruptBytes(t *testing.T) {
	_, _, err := deserializeSnapshot([]byte("definitely not json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deserialize")
}

// ---------------------------------------------------------------------------
// Rehydration from SnapshotValid messages.
// ---------------------------------------------------------------------------

// TestProcessApplyMsg_SnapshotRehydrates verifies the SnapshotValid path:
// wholesale replace of state + dedup, lastApplied/lastSnapshotIndex jump
// to LastIncludedIndex, recent cleared, and NO waiter traffic.
func TestProcessApplyMsg_SnapshotRehydrates(t *testing.T) {
	s := newTestServerWithLogger(t)

	s.mu.Lock()
	s.state["stale"] = "old" // must be replaced wholesale
	s.duplicates[999] = map[int64]*DuplicateEntry{5: {Timestamp: time.Now()}}
	s.recent[42] = Result{} // history cleared by snapshot
	s.mu.Unlock()

	msg := raft.ApplyMsg{
		SnapshotValid:     true,
		SnapshotData:      []byte(`{"state":{"k":"v"},"dedup":{"77":{"3":{}}}}`),
		LastIncludedIndex: 10,
		LastIncludedTerm:  4,
	}
	s.processApplyMsg(msg)

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, map[string]string{"k": "v"}, s.state)
	assert.Equal(t, int64(77), onlyClient(s.duplicates), "dedup replaced")
	assert.Empty(t, s.duplicates[999], "old dedup client gone")
	assert.Equal(t, 10, s.lastApplied)
	assert.Equal(t, 10, s.lastSnapshotIndex)
	assert.Empty(t, s.recent)
}

// ---------------------------------------------------------------------------
// Trigger: applied-entry count vs snapshot_threshold.
// ---------------------------------------------------------------------------

// TestApplyLoopTrigger_SnapshotsAtThreshold feeds three committed entries
// through the REAL pipeline (AppendEntries → raft applyLoop → applyCh →
// processApplyMsg) against a raft configured with threshold=2.
// Expect exactly one snapshot at index 2 covering keys k1..k2; entry 3
// stays un-snapshotted in the log tail, and heartbeats continue past the
// compacted base.
func TestApplyLoopTrigger_SnapshotsAtThreshold(t *testing.T) {
	persister := raft.NewMemoryPersister()
	rf, err := raft.NewRaft(raft.Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		SnapshotThreshold:  2,
		Persister:          persister,
	})
	require.NoError(t, err)
	// Long deterministic timeout so no election runs during the test.
	rf.SetDeterministicTimeout(10 * time.Second)
	rf.Start()
	defer rf.Shutdown()

	resp, err := rf.AppendEntries(context.Background(), aeReqWithEntries(0, 0,
		cmdEntry(1, cmdPut("k1", "v1")),
		cmdEntry(2, cmdPut("k2", "v2")),
		cmdEntry(3, cmdPut("k3", "v3")),
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())

	s := newTestServerWithRaft(t, rf)
	for i := 0; i < 3; i++ {
		select {
		case msg := <-rf.GetApplyCh():
			if msg.CommandValid {
				s.processApplyMsg(msg)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for committed ApplyMsg")
		}
	}

	data, idx, term, err := persister.LoadSnapshot()
	require.NoError(t, err, "trigger must have fired at threshold")
	assert.Equal(t, 2, idx, "snapshot covers entries 1..2")
	assert.Equal(t, 1, term)

	gotState, gotDedup, err := deserializeSnapshot(data)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k1": "v1", "k2": "v2"}, gotState)
	assert.Empty(t, gotDedup)

	s.mu.Lock()
	applied, snapIdx := s.lastApplied, s.lastSnapshotIndex
	s.mu.Unlock()
	assert.Equal(t, 3, applied)
	assert.Equal(t, 2, snapIdx)

	// The raft log was compacted: further appends continue past the base.
	next, err := rf.AppendEntries(context.Background(), aeReqWithoutEntries(1, 3, 1))
	require.NoError(t, err)
	require.True(t, next.GetSuccess(), "heartbeats past logBase must succeed")
}

// TestApplyLoopTrigger_DisabledAtZero pins the default: threshold 0 means
// snapshots never fire even after many applies.
func TestApplyLoopTrigger_DisabledAtZero(t *testing.T) {
	persister := raft.NewMemoryPersister()
	rf := newTestRaftForSnapshotWithPersister(t, 0, persister)
	rf.SetDeterministicTimeout(10 * time.Second)
	rf.Start()
	defer rf.Shutdown()

	_, err := rf.AppendEntries(context.Background(), aeReqWithEntries(0, 0,
		cmdEntry(1, cmdPut("k1", "v1")),
		cmdEntry(2, cmdPut("k2", "v2")),
		cmdEntry(3, cmdPut("k3", "v3")),
	))
	require.NoError(t, err)

	s := newTestServerWithRaft(t, rf)
	for i := 0; i < 3; i++ {
		msg := <-rf.GetApplyCh()
		if msg.CommandValid {
			s.processApplyMsg(msg)
		}
	}

	_, _, _, err = persister.LoadSnapshot()
	require.ErrorIs(t, err, raft.ErrNoSnapshot, "threshold 0 disables snapshots")

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, 3, s.lastApplied)
	assert.Zero(t, s.lastSnapshotIndex)
}
