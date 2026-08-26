// Whitebox tests for Raft.Snapshot (local snapshotting, PLAN.md Step S1b).
// Direct asserts on private fields (logBase, log, snapshot*) per AGENTS.md
// file-split convention.

package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSnapshotTestRaft builds a minimal raft node (no goroutines started)
// with a preset log of n entries all in term t, and commitIndex ==
// lastApplied == n. This mirrors the post-commit state on a healthy node.
func newSnapshotTestRaft(t *testing.T, n int, term int) *Raft {
	t.Helper()
	rf := newTestRaft(t)
	rf.mu.Lock()
	defer rf.mu.Unlock()
	for i := 1; i <= n; i++ {
		rf.log = append(rf.log, LogEntry{
			Index:   int64(i),
			Term:    int64(term),
			Command: []byte("cmd"),
		})
	}
	rf.commitIndex = n
	rf.lastApplied = n
	return rf
}

func TestRaft_Snapshot_TruncatesLogAndBumpsLogBase(t *testing.T) {
	const (
		n         = 5
		term      = 2
		snapIdx   = 3
		snapBytes = "snapshot-bytes"
	)
	rf := newSnapshotTestRaft(t, n, term)

	require.NoError(t, rf.Snapshot(snapIdx, []byte(snapBytes)))

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, snapIdx, rf.logBase, "logBase must advance to the snapshot index")
	assert.Equal(t, snapIdx, rf.snapshotIndex)
	assert.Equal(t, term, rf.snapshotTerm, "snapshot term = term of the entry at snapIdx")
	assert.Equal(t, []byte(snapBytes), rf.snapshot)
	require.Len(t, rf.log, n-snapIdx, "only entries past snapIdx remain")
	for i, e := range rf.log {
		assert.Equal(t, int64(snapIdx+1+i), e.Index)
	}
	assert.Equal(t, n, rf.commitIndex, "commitIndex unchanged")
	assert.Equal(t, n, rf.lastApplied, "lastApplied unchanged")

	// Durability invariant: both files reflect the snapshot.
	got, idx, gotTerm, err := rf.persister.LoadSnapshot()
	require.NoError(t, err)
	assert.Equal(t, snapIdx, idx)
	assert.Equal(t, term, gotTerm)
	assert.Equal(t, []byte(snapBytes), got)

	_, _, savedLog, err := rf.persister.Load()
	require.NoError(t, err)
	require.Len(t, savedLog, n-snapIdx, "raft-state.json persists only the tail")
}

// TestRaft_Snapshot_RejectsUncommitted enforces the safety rule: never
// truncate committed-but-unapplied (let alone uncommitted) territory.
func TestRaft_Snapshot_RejectsUncommitted(t *testing.T) {
	rf := newSnapshotTestRaft(t, 5, 1)
	rf.mu.Lock()
	rf.commitIndex = 3 // entry 5 exists locally but isn't committed
	rf.mu.Unlock()

	err := rf.Snapshot(5, []byte("nope"))
	require.Error(t, err)

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 0, rf.logBase, "failed snapshot must not mutate state")
	assert.Len(t, rf.log, 5)
}

// TestRaft_Snapshot_NoOpWhenBehindLogBase covers idempotence: asking for
// an index already covered by an earlier snapshot does nothing.
func TestRaft_Snapshot_NoOpWhenBehindLogBase(t *testing.T) {
	rf := newSnapshotTestRaft(t, 5, 1)
	require.NoError(t, rf.Snapshot(3, []byte("first")))

	before := func() *Raft { return rf } // capture for field compare below
	_ = before
	require.NoError(t, rf.Snapshot(2, []byte("older")))

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 3, rf.logBase, "stale request must not rewind logBase")
	assert.Equal(t, []byte("first"), rf.snapshot, "stale request must not overwrite snapshot")
}

// TestRaft_Snapshot_RejectsUnknownIndex guards against KV callers that
// drift out of sync with the log (e.g. after InstallSnapshot truncation).
func TestRaft_Snapshot_RejectsUnknownIndex(t *testing.T) {
	rf := newSnapshotTestRaft(t, 5, 1)

	err := rf.Snapshot(9, []byte("x")) // beyond the log entirely
	require.Error(t, err)

	// Index == logBase is a documented no-op (nothing to truncate), not
	// an error.
	require.NoError(t, rf.Snapshot(0, []byte("no-op")))
}

// ---------------------------------------------------------------------------
// Recovery via loadPersistedState (PLAN.md Step S1c).
// ---------------------------------------------------------------------------

// recvApplyMsg reads one ApplyMsg from rf's applyCh (with timeout guard so
// a regression can't hang the suite).
func recvApplyMsg(t *testing.T, rf *Raft) ApplyMsg {
	t.Helper()
	select {
	case msg := <-rf.GetApplyCh():
		return msg
	default:
		t.Fatal("expected a SnapshotValid ApplyMsg queued in applyCh")
		return ApplyMsg{}
	}
}

// TestLoadPersistedState_WithSnapshotAndTail verifies the hybrid recovery:
// snapshot covers 1..5, persisted log tail is 6..8. Expect logBase=5,
// commitIndex=8 (snapped to the tail), lastApplied=5 (snapshot counted),
// and one SnapshotValid ApplyMsg queued for the KV layer.
func TestLoadPersistedState_WithSnapshotAndTail(t *testing.T) {
	p := NewMemoryPersister()
	require.NoError(t, p.SaveSnapshot([]byte("state-through-5"), 5, 3))
	require.NoError(t, p.Save(9, -1, []LogEntry{
		{Index: 6, Term: 3, Command: []byte("cmd6")},
		{Index: 7, Term: 4, Command: []byte("cmd7")},
		{Index: 8, Term: 4, Command: []byte("cmd8")},
	}))

	rf, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          p,
	})
	require.NoError(t, err)

	rf.mu.Lock()
	logBase, snapIdx, snapTerm := rf.logBase, rf.snapshotIndex, rf.snapshotTerm
	commit, applied := rf.commitIndex, rf.lastApplied
	rf.mu.Unlock()

	assert.Equal(t, 5, logBase)
	assert.Equal(t, 5, snapIdx)
	assert.Equal(t, 3, snapTerm)
	assert.Equal(t, 8, commit, "commitIndex snaps to the persisted log tail")
	assert.Equal(t, 5, applied, "lastApplied floors at the snapshot index")

	msg := recvApplyMsg(t, rf)
	assert.True(t, msg.SnapshotValid)
	assert.False(t, msg.CommandValid, "command fields zero on snapshot messages")
	assert.Equal(t, 5, msg.LastIncludedIndex)
	assert.Equal(t, 3, msg.LastIncludedTerm)
	assert.Equal(t, []byte("state-through-5"), msg.SnapshotData)

	// Log slice arithmetic must line up with the new base.
	rf.mu.Lock()
	defer rf.mu.Unlock()
	require.Len(t, rf.log, 3)
	assert.Equal(t, int64(6), rf.log[0].Index, "array position 0 now maps to absolute index 6")
}

// TestLoadPersistedState_SnapshotOnly verifies the fully-compacted case:
// snapshot at 8, empty tail. commitIndex AND lastApplied floor at 8 (the
// snapshot itself counts as applied); nothing further to deliver besides
// the snapshot message.
func TestLoadPersistedState_SnapshotOnly(t *testing.T) {
	p := NewMemoryPersister()
	require.NoError(t, p.SaveSnapshot([]byte("state"), 8, 4))
	require.NoError(t, p.Save(10, -1, nil))

	rf, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          p,
	})
	require.NoError(t, err)

	rf.mu.Lock()
	commit, applied, logBase := rf.commitIndex, rf.lastApplied, rf.logBase
	rf.mu.Unlock()

	assert.Equal(t, 8, logBase)
	assert.Equal(t, 8, commit, "commitIndex must not rewind below the snapshot")
	assert.Equal(t, 8, applied, "fully-compacted node has nothing left to apply")

	msg := recvApplyMsg(t, rf)
	assert.True(t, msg.SnapshotValid)
	assert.Equal(t, 8, msg.LastIncludedIndex)
}

// TestLoadPersistedState_NoSnapshotKeepsLegacyFloor guards the 0.1.x
// behavior: without a snapshot, commitIndex and lastApplied still snap to
// the last log index (documented gap: KV map starts empty on restart —
// unchanged until snapshotting entered the picture).
func TestLoadPersistedState_NoSnapshotKeepsLegacyFloor(t *testing.T) {
	p := NewMemoryPersister()
	require.NoError(t, p.Save(2, 7, []LogEntry{
		{Index: 1, Term: 1, Command: []byte("a")},
		{Index: 2, Term: 2, Command: []byte("b")},
	}))

	rf, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          p,
	})
	require.NoError(t, err)

	rf.mu.Lock()
	commit, applied := rf.commitIndex, rf.lastApplied
	rf.mu.Unlock()

	assert.Equal(t, 2, commit)
	assert.Equal(t, 2, applied, "legacy floor: both snap to last log index")

	// No snapshot message queued for a snapshotless node.
	select {
	case msg := <-rf.GetApplyCh():
		t.Fatalf("unexpected ApplyMsg %#v", msg)
	default:
	}
}
