// Whitebox tests for Raft.Snapshot (local snapshotting, PLAN.md Step S1b).
// Direct asserts on private fields (logBase, log, snapshot*) per AGENTS.md
// file-split convention.

package raft

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
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

// ---------------------------------------------------------------------------
// InstallSnapshot handler (follower side) — PLAN.md Step S2.
// ---------------------------------------------------------------------------

// newFollowerRaft builds a follower with term `term`, a log of n entries
// in that term, and commitIndex == lastApplied == n.
func newFollowerRaft(t *testing.T, n int, term int) *Raft {
	t.Helper()
	rf := newTestRaft(t)
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.currentTerm = term
	for i := 1; i <= n; i++ {
		rf.log = append(rf.log, LogEntry{Index: int64(i), Term: int64(term), Command: []byte("cmd")})
	}
	rf.commitIndex = n
	rf.lastApplied = n
	return rf
}

func installReq(term, leaderId, lii, lit int, data []byte) *raftpb.InstallSnapshotRequest {
	return &raftpb.InstallSnapshotRequest{
		Term:              int64(term),
		LeaderId:          int32(leaderId),
		LastIncludedIndex: int64(lii),
		LastIncludedTerm:  int64(lit),
		Data:              data,
		Done:              true,
	}
}

func TestInstallSnapshot_StaleTermRejected(t *testing.T) {
	rf := newFollowerRaft(t, 5, 7)

	resp, err := rf.InstallSnapshot(context.Background(), installReq(3, 2, 4, 1, []byte("old")))
	require.NoError(t, err)
	assert.Equal(t, int64(7), resp.GetTerm())

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 0, rf.logBase, "stale leader must not install")
	assert.Len(t, rf.log, 5)
}

func TestInstallSnapshot_HigherTermStepsDownAndInstalls(t *testing.T) {
	rf := newFollowerRaft(t, 5, 2)

	resp, err := rf.InstallSnapshot(context.Background(), installReq(9, 3, 8, 4, []byte("state")))
	require.NoError(t, err)
	assert.Equal(t, int64(9), resp.GetTerm())

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, Follower, rf.state)
	assert.Equal(t, 8, rf.logBase)
	assert.Equal(t, 9, rf.currentTerm)
	assert.Equal(t, 3, rf.leaderId)
	assert.Equal(t, 8, rf.commitIndex)
	assert.Equal(t, 8, rf.lastApplied)

	msg := recvApplyMsg(t, rf)
	assert.True(t, msg.SnapshotValid)
	assert.Equal(t, 8, msg.LastIncludedIndex)
	assert.Equal(t, []byte("state"), msg.SnapshotData)
}

func TestInstallSnapshot_KeepsTailPastSnapshot(t *testing.T) {
	rf := newFollowerRaft(t, 10, 1)
	// Follower knows about entries up to 10 but has only committed 2:
	// behind enough for the snapshot to matter.
	rf.mu.Lock()
	rf.commitIndex, rf.lastApplied = 2, 2
	rf.mu.Unlock()

	_, err := rf.InstallSnapshot(context.Background(), installReq(5, 2, 6, 1, []byte("s")))
	require.NoError(t, err)

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 6, rf.logBase)
	require.Len(t, rf.log, 4, "entries 7..10 survive")
	assert.Equal(t, int64(7), rf.log[0].Index)
	assert.Equal(t, 6, rf.commitIndex)
	assert.Equal(t, 6, rf.lastApplied, "floors at snapshot; tail replays via applyLoop")
}

func TestInstallSnapshot_DiscardsWhenAlreadyApplied(t *testing.T) {
	rf := newFollowerRaft(t, 5, 3)
	rf.mu.Lock()
	rf.commitIndex = 10
	rf.lastApplied = 10
	rf.mu.Unlock()

	_, err := rf.InstallSnapshot(context.Background(), installReq(3, 2, 8, 3, []byte("late")))
	require.NoError(t, err)

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 0, rf.logBase, "already past this snapshot — discard")
	assert.Len(t, rf.log, 5)
}

func TestInstallSnapshot_TermMismatchDiscards(t *testing.T) {
	// Local log claims index 5 is term 9 but the snapshot says term 1:
	// divergent histories, refuse to clobber (CondInstallSnapshot rule).
	rf := newFollowerRaft(t, 5, 9)

	resp, err := rf.InstallSnapshot(context.Background(), installReq(9, 2, 5, 1, []byte("divergent")))
	require.NoError(t, err)
	assert.Equal(t, int64(9), resp.GetTerm())
	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 0, rf.logBase)
	assert.Len(t, rf.log, 5)
}

func TestInstallSnapshot_AcceptsWhenLogTooShortToVerify(t *testing.T) {
	// Empty log can't contradict the snapshot: must accept (the only way
	// a far-behind node ever catches up).
	rf := newTestRaft(t)
	rf.mu.Lock()
	rf.currentTerm = 1
	rf.mu.Unlock()

	resp, err := rf.InstallSnapshot(context.Background(), installReq(5, 2, 12, 3, []byte("far")))
	require.NoError(t, err)
	assert.Equal(t, int64(5), resp.GetTerm())

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 12, rf.logBase)
	assert.Empty(t, rf.log)
	assert.Equal(t, 12, rf.commitIndex)
}

func TestInstallSnapshot_OversizeRejected(t *testing.T) {
	rf := newFollowerRaft(t, 5, 1)

	big := make([]byte, maxSnapshotBytes+1)
	_, err := rf.InstallSnapshot(context.Background(), installReq(1, 2, 3, 1, big))
	require.Error(t, err)

	rf.mu.Lock()
	defer rf.mu.Unlock()
	assert.Equal(t, 0, rf.logBase, "oversize must not install")
}

func TestInstallSnapshot_PersistsBeforeRespond(t *testing.T) {
	p := NewMemoryPersister()
	rf, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          p,
	})
	require.NoError(t, err)
	p.SetSaveErr(fmt.Errorf("disk full"))

	_, err = rf.InstallSnapshot(context.Background(), installReq(1, 2, 3, 1, []byte("s")))
	require.Error(t, err, "persist failure must surface to the leader")

	data, idx, _, loadErr := p.LoadSnapshot()
	if loadErr == nil {
		assert.NotEqual(t, []byte("s"), data, "nothing durable should be reported installed")
	}
	_ = idx
}

// TestSendAppendEntries_FallsBackToInstallSnapshot exercises the real
// wire path: leader with logBase=2, peer.nextIndex=1 → InstallSnapshot
// delivered over live gRPC, follower installs, leader advances peer state.
func TestSendAppendEntries_FallsBackToInstallSnapshot(t *testing.T) {
	// Follower B on a real listener.
	lisB, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	follower, err := NewRaft(Config{
		ServerID:           2,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
	})
	require.NoError(t, err)
	srvB := grpc.NewServer()
	raftpb.RegisterRaftServer(srvB, follower)
	go func() { _ = srvB.Serve(lisB) }()
	defer srvB.GracefulStop()

	// Leader A pointed at B.
	leader, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{2: lisB.Addr().String()},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
	})
	require.NoError(t, err)
	leader.SetDeterministicTimeout(10 * time.Second)
	leader.Start()
	defer leader.Shutdown()

	const term = 4
	leader.mu.Lock()
	leader.state = Leader
	leader.currentTerm = term
	for i := 1; i <= 4; i++ {
		leader.log = append(leader.log, LogEntry{Index: int64(i), Term: int64(term), Command: []byte("c")})
	}
	leader.commitIndex, leader.lastApplied = 4, 4
	leader.peers[2].nextIndex = 1 // so far behind it predates the upcoming snapshot
	leader.mu.Unlock()

	require.NoError(t, leader.Snapshot(2, []byte("compacted-state")))

	// prevLogIdx=0 < logBase=2 → fallback kicks in. The snapshot ships
	// from a spawned goroutine, so poll rather than assume completion.
	go leader.sendAppendEntries(2, term, 4)

	require.Eventually(t, func() bool {
		leader.mu.Lock()
		defer leader.mu.Unlock()
		return leader.peers[2].matchIndex == 2 && leader.peers[2].nextIndex == 3
	}, 5*time.Second, 10*time.Millisecond, "leader should record the install")

	// Follower actually installed + queued rehydration.
	require.Eventually(t, func() bool {
		follower.mu.Lock()
		defer follower.mu.Unlock()
		return follower.logBase == 2 && follower.snapshotIndex == 2
	}, 5*time.Second, 10*time.Millisecond, "follower should install the snapshot")

	follower.mu.Lock()
	data := append([]byte(nil), follower.snapshot...)
	follower.mu.Unlock()
	assert.Equal(t, []byte("compacted-state"), data)
	assert.Equal(t, term, follower.CurrentTerm(), "follower adopted the leader's term")

	msg := recvApplyMsg(t, follower)
	assert.True(t, msg.SnapshotValid)
	assert.Equal(t, 2, msg.LastIncludedIndex)
}
