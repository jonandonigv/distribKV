package raft

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// ---------------------------------------------------------------------------
// Test harness: real in-process gRPC on ephemeral ports.
//
// newTestCluster spins N real gRPC servers on 127.0.0.1:0 ephemeral ports,
// constructs N Raft nodes with explicit deterministic IDs, registers each
// node as a raftpb.RaftServer, and starts serving. Call start() to launch
// election timers; call shutdown() to stop everything cleanly.
//
// The harness is whitebox (package raft) so it can call SetDeterministicTimeout
// and read private state for assertions.
// ---------------------------------------------------------------------------

type testCluster struct {
	t        *testing.T
	n        int
	rafts    []*Raft
	servers  []*grpc.Server
	whiles   []net.Listener
	applyChs []chan ApplyMsg
	dataDirs []string
	started  bool
}

// newTestCluster creates N nodes on ephemeral ports with deterministic IDs.
// Each node uses a MemoryPersister (no disk I/o) and a test-safe deterministic
// election timeout if opts specify one. Does NOT call Start — use start().
func newTestCluster(t *testing.T, n int, timeout time.Duration) *testCluster {
	t.Helper()
	require.GreaterOrEqual(t, n, 1, "cluster must have at least one node")

	addrs := make(map[int]string, n)
	listeners := make([]net.Listener, n)
	for i := 0; i < n; i++ {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err, "bind listener for node %d", i)
		listeners[i] = lis
		addrs[i] = lis.Addr().String()
	}

	tc := &testCluster{
		t:        t,
		n:        n,
		rafts:    make([]*Raft, n),
		servers:  make([]*grpc.Server, n),
		whiles:   listeners,
		applyChs: make([]chan ApplyMsg, n),
	}

	for i := 0; i < n; i++ {
		peers := make(map[int]string)
		for j := 0; j < n; j++ {
			if j != i {
				peers[j] = addrs[j]
			}
		}

		cfg := Config{
			ServerID:           i,
			OwnAddr:            addrs[i],
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  50 * time.Millisecond,
			Persister:          NewMemoryPersister(),
			Logger:             slog.New(slog.NewTextHandler(&testLogWriter{t: t}, nil)),
		}
		if timeout > 0 {
			cfg.ElectionTimeoutMin = timeout
			cfg.ElectionTimeoutMax = timeout + 50*time.Millisecond // max > min for validation; randomized range breaks split votes
		}

		rf, err := NewRaft(cfg)
		require.NoError(t, err, "NewRaft node %d", i)
		// Only use a fixed deterministic timeout for single-node tests
		// (no split-vote risk). Multi-node needs randomization to avoid
		// permanent split votes.
		if timeout > 0 && n == 1 {
			rf.SetDeterministicTimeout(timeout)
		}
		tc.rafts[i] = rf
		tc.applyChs[i] = rf.GetApplyCh()
	}

	for i := 0; i < n; i++ {
		srv := grpc.NewServer()
		raftpb.RegisterRaftServer(srv, tc.rafts[i])
		tc.servers[i] = srv
		go func(i int) { _ = tc.servers[i].Serve(tc.whiles[i]) }(i)
	}

	return tc
}

func (tc *testCluster) start() {
	tc.t.Helper()
	for _, rf := range tc.rafts {
		rf.Start()
	}
	tc.started = true
}

func (tc *testCluster) shutdown() {
	tc.t.Helper()
	for _, rf := range tc.rafts {
		rf.Shutdown()
	}
	for i, srv := range tc.servers {
		if srv != nil {
			srv.GracefulStop()
			tc.servers[i] = nil
		}
	}
}

func (tc *testCluster) leaderCount() int {
	count := 0
	for _, rf := range tc.rafts {
		if rf.IsLeader() {
			count++
		}
	}
	return count
}

func (tc *testCluster) waitLeader(timeout time.Duration) *Raft {
	tc.t.Helper()
	var leader *Raft
	require.Eventuallyf(tc.t, func() bool {
		count := 0
		for _, rf := range tc.rafts {
			if rf.IsLeader() {
				leader = rf
				count++
			}
		}
		return count == 1
	}, timeout, 10*time.Millisecond, "expected exactly one leader within %v", timeout)
	return leader
}

// testLogWriter routes slog output through t.Log so test output is
// attributed to the right test rather than spilling to stderr.
type testLogWriter struct {
	t *testing.T
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// ---------------------------------------------------------------------------
// Whitebox election unit tests — direct handler/method calls, no gRPC.
// ---------------------------------------------------------------------------

// newTestRaft builds a single Raft node for unit tests. No peers, no
// gRPC server; just the struct and persister ready for direct handler
// invocation.
func newTestRaft(t *testing.T) *Raft {
	t.Helper()
	rf, err := NewRaft(Config{
		ServerID:           1,
		OwnAddr:            "127.0.0.1:0",
		Peers:              map[int]string{},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
		Logger:             slog.New(slog.NewTextHandler(&testLogWriter{t: t}, nil)),
	})
	require.NoError(t, err)
	return rf
}

func TestRequestVote_GrantsVote_HigherTerm(t *testing.T) {
	rf := newTestRaft(t)
	resp, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         1,
		CandidateId:  2,
		LastLogIndex: 0,
		LastLogTerm:  0,
	})
	require.NoError(t, err)
	require.True(t, resp.GetVoteGranted(), "should grant vote for higher term")
	require.Equal(t, int64(1), resp.GetTerm())
	require.Equal(t, 2, rf.VotedFor(), "votedFor should be candidate 2")
	require.Equal(t, 1, rf.CurrentTerm(), "term should advance to 1")
}

func TestRequestVote_GrantsVote_SameTermNotYetVoted(t *testing.T) {
	rf := newTestRaft(t)
	// Manually set term to 1 to test same-term scenario.
	rf.mu.Lock()
	rf.currentTerm = 1
	rf.mu.Unlock()

	resp, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         1,
		CandidateId:  3,
		LastLogIndex: 0,
		LastLogTerm:  0,
	})
	require.NoError(t, err)
	require.True(t, resp.GetVoteGranted(), "should grant vote when not yet voted this term")
}

func TestRequestVote_Rejects_StaleTerm(t *testing.T) {
	rf := newTestRaft(t)
	rf.mu.Lock()
	rf.currentTerm = 5
	rf.mu.Unlock()

	resp, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         3,
		CandidateId:  2,
		LastLogIndex: 0,
		LastLogTerm:  0,
	})
	require.NoError(t, err)
	require.False(t, resp.GetVoteGranted(), "should reject stale term")
	require.Equal(t, int64(5), resp.GetTerm(), "should return currentTerm")
}

func TestRequestVote_Rejects_AlreadyVoted(t *testing.T) {
	rf := newTestRaft(t)
	// Simulate already voted for candidate 2 in term 1.
	rf.mu.Lock()
	rf.currentTerm = 1
	rf.votedFor = 2
	rf.mu.Unlock()

	resp, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         1,
		CandidateId:  3,
		LastLogIndex: 0,
		LastLogTerm:  0,
	})
	require.NoError(t, err)
	require.False(t, resp.GetVoteGranted(), "should reject when already voted for someone else")
}

func TestRequestVote_Rejects_LogNotUpToDate(t *testing.T) {
	rf := newTestRaft(t)
	// Give the node a log entry at term 2, index 1.
	rf.mu.Lock()
	rf.currentTerm = 2
	rf.log = []LogEntry{{Index: 1, Term: 2, Command: []byte("x")}}
	rf.mu.Unlock()

	// Candidate with older log (term 1 < 2).
	resp, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         3,
		CandidateId:  5,
		LastLogIndex: 1,
		LastLogTerm:  1, // older than our term 2
	})
	require.NoError(t, err)
	require.False(t, resp.GetVoteGranted(), "should reject candidate with stale log")

	// Candidate with same term but shorter log.
	resp2, err := rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:         3,
		CandidateId:  5,
		LastLogIndex: 0, // shorter than our index 1
		LastLogTerm:  2,
	})
	require.NoError(t, err)
	require.False(t, resp2.GetVoteGranted(), "should reject candidate with shorter log at same term")
}

func TestRequestVote_PersistsBeforeRespond(t *testing.T) {
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

	// Inject a persist failure — the handler should return an error,
	// NOT grant the vote, proving the persist happens before the response.
	p.SetSaveErr(fmt.Errorf("disk full"))

	_, err = rf.RequestVote(context.Background(), &raftpb.RequestVoteRequest{
		Term:        1,
		CandidateId: 2,
	})
	require.Error(t, err, "handler should return error when persist fails")
	require.Equal(t, -1, rf.VotedFor(), "votedFor must not update when persist fails")
}

func TestBecomeCandidate_TransitionsAndIncrementsTerm(t *testing.T) {
	// Use a node WITH peers so becomeCandidate stays as Candidate (dispatches
	// RequestVote RPCs that will fail harmlessly — the node can't reach
	// majority without real gRPC peers). A peerless node would self-elect
	// to Leader immediately.
	rf, err := NewRaft(Config{
		ServerID:           1,
		OwnAddr:            "127.0.0.1:10001",
		Peers:              map[int]string{2: "127.0.0.1:10002"},
		ElectionTimeoutMin: 150 * time.Millisecond,
		ElectionTimeoutMax: 300 * time.Millisecond,
		HeartbeatInterval:  50 * time.Millisecond,
		Persister:          NewMemoryPersister(),
		Logger:             slog.New(slog.NewTextHandler(&testLogWriter{t: t}, nil)),
	})
	require.NoError(t, err)
	rf.SetDeterministicTimeout(10 * time.Second) // won't fire during test
	rf.Start()
	defer rf.Shutdown()

	rf.mu.Lock()
	rf.currentTerm = 3
	rf.mu.Unlock()

	rf.becomeCandidate()

	require.Equal(t, Candidate, rf.State())
	require.Equal(t, 4, rf.CurrentTerm(), "term should increment")
	require.Equal(t, 1, rf.VotedFor(), "should vote for self")
	require.Equal(t, -1, rf.GetLeaderId(), "leaderId should be cleared")
}

func TestBecomeLeader_InitializesPeerState(t *testing.T) {
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

	// Put the node in Candidate state with a log.
	rf.mu.Lock()
	rf.state = Candidate
	rf.currentTerm = 2
	rf.log = []LogEntry{{Index: 1, Term: 2, Command: []byte("x")}}
	rf.mu.Unlock()

	// Start is needed for becomeLeader to start the heartbeat sender
	// (which needs the ctx). We'll use a short deterministic timeout.
	rf.SetDeterministicTimeout(10 * time.Second) // won't fire during test
	rf.Start()
	defer rf.Shutdown()

	rf.becomeLeader()

	require.Equal(t, Leader, rf.State())
	require.Equal(t, 1, rf.GetLeaderId(), "leaderId should be self")

	// nextIndex should be len(log)+1 = 2 for each peer.
	rf.mu.Lock()
	for _, p := range rf.peers {
		require.Equal(t, 2, p.nextIndex, "nextIndex should be len(log)+1")
		require.Equal(t, 0, p.matchIndex, "matchIndex should start at 0")
	}
	rf.mu.Unlock()
}

func TestStepDown_TransitionsToFollower(t *testing.T) {
	rf := newTestRaft(t)
	rf.mu.Lock()
	rf.state = Candidate
	rf.currentTerm = 3
	rf.votedFor = 1
	rf.mu.Unlock()

	rf.mu.Lock()
	rf.stepDown(5)
	rf.mu.Unlock()

	require.Equal(t, Follower, rf.State())
	require.Equal(t, 5, rf.CurrentTerm(), "term should update to new term")
	require.Equal(t, -1, rf.VotedFor(), "votedFor should be reset")
	require.Equal(t, -1, rf.GetLeaderId(), "leaderId should be cleared")
}

// ---------------------------------------------------------------------------
// Harness-based integration tests — real in-process gRPC, multi-node.
// ---------------------------------------------------------------------------

func TestElection_SingleNodeElectsSelf(t *testing.T) {
	tc := newTestCluster(t, 1, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	leader := tc.waitLeader(2 * time.Second)
	require.NotNil(t, leader)
	require.Equal(t, 0, leader.GetServerId(), "single-node leader must be node 0")
	require.Equal(t, 0, leader.GetLeaderId(), "GetLeaderId returns own id when leader")
}

func TestElection_3NodesElectExactlyOneLeader(t *testing.T) {
	tc := newTestCluster(t, 3, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	leader := tc.waitLeader(5 * time.Second)
	require.NotNil(t, leader)
	require.Equal(t, 1, tc.leaderCount(), "expected exactly one leader")
}

func TestElection_ReElectionAfterLeaderShutdown(t *testing.T) {
	tc := newTestCluster(t, 3, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	// Wait for initial leader.
	leader := tc.waitLeader(5 * time.Second)
	leaderId := leader.GetServerId()

	// Shutdown the leader node's Raft (but keep the gRPC server up so
	// the port stays bound — the other nodes' connections will fail and
	// they'll time out and re-elect).
	leader.Shutdown()

	// The remaining two nodes should elect a new leader.
	require.Eventuallyf(t, func() bool {
		// Must not be the old leader and must be exactly one.
		count := 0
		newId := -1
		for _, rf := range tc.rafts {
			if rf.IsLeader() {
				newId = rf.GetServerId()
				count++
			}
		}
		return count == 1 && newId != leaderId
	}, 5*time.Second, 10*time.Millisecond,
		"expected a new leader (not %d) after old leader shutdown", leaderId)
}

func TestElection_NoSplitBrain(t *testing.T) {
	tc := newTestCluster(t, 5, 200*time.Millisecond)
	tc.start()
	defer tc.shutdown()

	tc.waitLeader(5 * time.Second)

	// Assert that at no point over a 500ms window do we see >1 leader.
	require.Neverf(t, func() bool {
		return tc.leaderCount() > 1
	}, 500*time.Millisecond, 10*time.Millisecond, "detected split brain (>1 leader)")
}

// ---------------------------------------------------------------------------
// Start / Shutdown lifecycle tests.
// ---------------------------------------------------------------------------

func TestStart_ShutdownIsIdempotent(t *testing.T) {
	rf := newTestRaft(t)
	rf.SetDeterministicTimeout(10 * time.Second) // won't fire
	rf.Start()
	require.NotPanics(t, func() {
		rf.Shutdown()
		rf.Shutdown() // double shutdown is safe
	})
}

func TestStart_CompletesQuickly(t *testing.T) {
	rf := newTestRaft(t)
	rf.SetDeterministicTimeout(10 * time.Second)
	rf.Start()
	done := make(chan struct{})
	go func() {
		rf.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not complete within 3s; goroutines leaked")
	}
}
