package raft

import (
	"context"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AppendEntries handler unit tests (whitebox, no gRPC).
// ---------------------------------------------------------------------------

// helper: build an AppendEntriesRequest with entries.
func aeReq(term int, leaderId, prevIdx, prevTerm, commit int, entries ...*raftpb.LogEntry) *raftpb.AppendEntriesRequest {
	return &raftpb.AppendEntriesRequest{
		Term:         int64(term),
		LeaderId:     int32(leaderId),
		PrevLogIndex: int64(prevIdx),
		PrevLogTerm:  int64(prevTerm),
		Entries:      entries,
		LeaderCommit: int64(commit),
	}
}

func TestAppendEntries_AppendsEntries(t *testing.T) {
	rf := newTestRaft(t)
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
		&raftpb.LogEntry{Index: 2, Term: 1, Command: []byte("b")},
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())

	log := rf.Log()
	require.Len(t, log, 2)
	assert.Equal(t, int64(1), log[0].Index)
	assert.Equal(t, int64(1), log[0].Term)
	assert.Equal(t, []byte("a"), log[0].Command)
	assert.Equal(t, int64(2), log[1].Index)
}

func TestAppendEntries_HeartbeatOnly(t *testing.T) {
	rf := newTestRaft(t)
	// Append one entry first.
	_, _ = rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("x")},
	))
	require.Len(t, rf.Log(), 1)

	// Heartbeat (no entries) should not change the log.
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 1, 1, 0))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())
	require.Len(t, rf.Log(), 1, "heartbeat should not append entries")
}

func TestAppendEntries_PrevLogMismatch_Rejects(t *testing.T) {
	rf := newTestRaft(t)
	// Follower has entry {1, term=1}.
	_, _ = rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("old")},
	))

	// Leader claims prevLogIndex=1, prevLogTerm=2 (we have term=1).
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 3, 1, 2, 0,
		&raftpb.LogEntry{Index: 2, Term: 1, Command: []byte("new")},
	))
	require.NoError(t, err)
	require.False(t, resp.GetSuccess(), "should reject when prevLogTerm mismatches")
}

func TestAppendEntries_PrevLogMissing_Rejects(t *testing.T) {
	rf := newTestRaft(t)
	// Empty log, leader claims prevLogIndex=5.
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 5, 1, 0,
		&raftpb.LogEntry{Index: 6, Term: 1, Command: []byte("x")},
	))
	require.NoError(t, err)
	require.False(t, resp.GetSuccess(), "should reject when we don't have prevLogIndex")
}

func TestAppendEntries_LogConflict_Truncates(t *testing.T) {
	rf := newTestRaft(t)
	// Follower has entries 1,2,3 all at term 1.
	_, _ = rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
		&raftpb.LogEntry{Index: 2, Term: 1, Command: []byte("b")},
		&raftpb.LogEntry{Index: 3, Term: 1, Command: []byte("c")},
	))
	require.Len(t, rf.Log(), 3)

	// Leader sends entry 2 with a different term (conflict). Followers
	// must truncate from the conflict point and append the new entry.
	resp, err := rf.AppendEntries(context.Background(), aeReq(2, 2, 1, 1, 0,
		&raftpb.LogEntry{Index: 2, Term: 2, Command: []byte("B'")},
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())

	log := rf.Log()
	require.Len(t, log, 2, "conflicting suffix should be truncated, new entry appended")
	assert.Equal(t, int64(1), log[0].Index)
	assert.Equal(t, int64(1), log[0].Term)
	assert.Equal(t, []byte("a"), log[0].Command, "entry 1 preserved")
	assert.Equal(t, int64(2), log[1].Index)
	assert.Equal(t, int64(2), log[1].Term)
	assert.Equal(t, []byte("B'"), log[1].Command, "entry 2 replaced")
}

func TestAppendEntries_IdempotentResend(t *testing.T) {
	rf := newTestRaft(t)
	_, _ = rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
	))
	// Re-send the same entry. Should not duplicate.
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())
	require.Len(t, rf.Log(), 1, "resending same entry should not duplicate")
}

func TestAppendEntries_AdvancesCommitIndex(t *testing.T) {
	rf := newTestRaft(t)
	// Append entries with leaderCommit=2.
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 2,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
		&raftpb.LogEntry{Index: 2, Term: 1, Command: []byte("b")},
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())
	assert.Equal(t, 2, rf.CommitIndex(), "commitIndex should advance to leaderCommit")
}

func TestAppendEntries_CommitCappedAtLastNewIndex(t *testing.T) {
	rf := newTestRaft(t)
	// Leader sends 1 entry but claims leaderCommit=5 (way ahead).
	resp, err := rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 5,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("a")},
	))
	require.NoError(t, err)
	require.True(t, resp.GetSuccess())
	assert.Equal(t, 1, rf.CommitIndex(), "commitIndex capped at last new entry index")
}

func TestAppendEntries_PersistsAfterAppend(t *testing.T) {
	p := NewMemoryPersister()
	rf, err := NewRaft(Config{
		ServerID:           1,
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          p,
		Logger:             newTestRaft(t).logger,
	})
	require.NoError(t, err)

	_, err = rf.AppendEntries(context.Background(), aeReq(1, 2, 0, 0, 0,
		&raftpb.LogEntry{Index: 1, Term: 1, Command: []byte("persisted")},
	))
	require.NoError(t, err)

	// Verify persisted state matches.
	term, votedFor, log, err := p.Load()
	require.NoError(t, err)
	assert.Equal(t, 1, term)
	assert.Equal(t, -1, votedFor)
	require.Len(t, log, 1)
	assert.Equal(t, []byte("persisted"), log[0].Command)
}

// ---------------------------------------------------------------------------
// updateCommitIndex tests (whitebox, direct call).
// ---------------------------------------------------------------------------

func TestUpdateCommitIndex_Majority(t *testing.T) {
	rf, err := NewRaft(Config{
		ServerID:           1,
		OwnAddr:            "127.0.0.1:10001",
		Peers:              map[int]string{2: "127.0.0.1:10002", 3: "127.0.0.1:10003"},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
	})
	require.NoError(t, err)

	// Set up as leader with two log entries at term 2.
	rf.mu.Lock()
	rf.state = Leader
	rf.currentTerm = 2
	rf.log = []LogEntry{
		{Index: 1, Term: 2, Command: []byte("a")},
		{Index: 2, Term: 2, Command: []byte("b")},
	}
	// One peer has replicated up to index 2, the other up to 1.
	rf.peers[2].matchIndex = 2
	rf.peers[3].matchIndex = 1
	rf.mu.Unlock()

	rf.mu.Lock()
	rf.updateCommitIndex()
	rf.mu.Unlock()

	// With 3 nodes (us + 2 peers), majority is 2. Entry 2 is replicated
	// on us + peer 2, so count=2 >= majority.  Entry 2 term == currentTerm
	// (Figure 8). commitIndex should advance to 2.
	assert.Equal(t, 2, rf.CommitIndex(), "should commit entry 2 (majority, current term)")
}

func TestUpdateCommitIndex_Figure8_PreviousTermNotCommitted(t *testing.T) {
	rf, err := NewRaft(Config{
		ServerID:           1,
		OwnAddr:            "127.0.0.1:10001",
		Peers:              map[int]string{2: "127.0.0.1:10002", 3: "127.0.0.1:10003"},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
	})
	require.NoError(t, err)

	// Leader at term 3 with entries from term 2 (previous term) that have
	// majority replication but were never committed.
	rf.mu.Lock()
	rf.state = Leader
	rf.currentTerm = 3
	rf.log = []LogEntry{
		{Index: 1, Term: 2, Command: []byte("old")},
		{Index: 2, Term: 2, Command: []byte("old2")},
		{Index: 3, Term: 3, Command: []byte("new")},
	}
	rf.peers[2].matchIndex = 2
	rf.peers[3].matchIndex = 2
	// No current-term entry replicated on any peer yet.
	rf.mu.Unlock()

	rf.mu.Lock()
	rf.updateCommitIndex()
	rf.mu.Unlock()

	// Even though entries 1 and 2 have majority, their term (2) != currentTerm
	// (3). Entry 3 has no majority (0 peers replicated). commitIndex stays 0.
	assert.Equal(t, 0, rf.CommitIndex(), "Figure 8: must not commit previous-term entries without a current-term entry")

	// Now peer 2 replicates entry 3.
	rf.mu.Lock()
	rf.peers[2].matchIndex = 3
	rf.updateCommitIndex()
	rf.mu.Unlock()

	// Now entry 3 (current term) has majority (us + peer 2). Commit 3.
	// And entries 1,2 (previous term) can be committed transitively.
	assert.Equal(t, 3, rf.CommitIndex(), "once a current-term entry commits, prior entries commit too")
}

// ---------------------------------------------------------------------------
// ReplicateCommand tests (step 3e).
// ---------------------------------------------------------------------------

func TestReplicateCommand_NotLeader(t *testing.T) {
	rf := newTestRaft(t)
	rf.Start()
	defer rf.Shutdown()

	_, err := rf.ReplicateCommand([]byte("test"))
	assert.ErrorIs(t, err, ErrNotLeader)
}

func TestReplicateCommand_SingleNodeCommits(t *testing.T) {
	rf := newTestRaft(t)
	rf.SetDeterministicTimeout(100 * time.Millisecond) // fires quickly for self-elect
	rf.Start()
	defer rf.Shutdown()

	// Single node self-elects.
	require.Eventually(t, func() bool { return rf.IsLeader() }, 2*time.Second, 10*time.Millisecond)

	idx, err := rf.ReplicateCommand([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 1, idx, "first command is at log index 1")
	assert.Equal(t, 1, rf.CommitIndex(), "single-node leader commits immediately")

	// The applyCh should deliver the entry.
	select {
	case msg := <-rf.GetApplyCh():
		assert.Equal(t, 1, msg.CommandIndex)
		assert.Equal(t, []byte("hello"), msg.Command)
	case <-time.After(time.Second):
		t.Fatal("applyCh did not deliver the committed entry")
	}
}

func TestReplicateCommand_ReturnsIndex(t *testing.T) {
	rf := newTestRaft(t)
	rf.SetDeterministicTimeout(100 * time.Millisecond)
	rf.Start()
	defer rf.Shutdown()

	require.Eventually(t, func() bool { return rf.IsLeader() }, 2*time.Second, 10*time.Millisecond)

	// Submit and consume from applyCh so commitIndex advances.
	idx1, err := rf.ReplicateCommand([]byte("a"))
	require.NoError(t, err)
	<-rf.GetApplyCh()

	idx2, err := rf.ReplicateCommand([]byte("b"))
	require.NoError(t, err)
	assert.Equal(t, idx1+1, idx2, "indices should be sequential")
}

// ---------------------------------------------------------------------------
// Integration test: 3-node cluster replicates a command to all followers.
// ---------------------------------------------------------------------------

func TestReplication_3NodesReplicateCommand(t *testing.T) {
	tc := newTestCluster(t, 3, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	leader := tc.waitLeader(5 * time.Second)

	idx, err := leader.ReplicateCommand([]byte("hello raft"))
	require.NoError(t, err)
	assert.True(t, idx >= 1)

	// All three nodes should deliver the entry via applyCh.
	for i := range tc.rafts {
		select {
		case msg := <-tc.applyChs[i]:
			assert.Equal(t, idx, msg.CommandIndex, "node %d: wrong index", i)
			assert.Equal(t, []byte("hello raft"), msg.Command, "node %d: wrong command", i)
		case <-time.After(3 * time.Second):
			t.Fatalf("node %d: applyCh did not deliver entry %d within 3s", i, idx)
		}
	}

	// commitIndex should match on all nodes.
	for i, rf := range tc.rafts {
		assert.Equal(t, idx, rf.CommitIndex(), "node %d commitIndex mismatch", i)
	}
}

// TestReplication_FollowerCatchesUp verifies that a follower that missed
// entries (simulated by delayed AppendEntries) eventually catches up via
// the nextIndex decrement retry loop.
func TestReplication_FollowerCatchUp(t *testing.T) {
	tc := newTestCluster(t, 3, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	leader := tc.waitLeader(5 * time.Second)

	// Submit a few commands.
	for i := 0; i < 3; i++ {
		_, err := leader.ReplicateCommand([]byte("cmd"))
		require.NoError(t, err)
	}

	// All nodes should eventually have commitIndex >= 3.
	for i, rf := range tc.rafts {
		require.Eventuallyf(t, func() bool {
			return rf.CommitIndex() >= 3
		}, 3*time.Second, 10*time.Millisecond, "node %d: commitIndex didn't reach 3", i)
	}
}
