package raft

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/jonandonigv/distribKV/proto/raft"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// testCluster spins a real in-process gRPC Raft cluster on ephemeral loopback
// ports with explicit deterministic node ids. It is the foundation for all
// whitebox raft tests.
//
// Lifecycle:
//
//	tc := newTestCluster(t, 3)   // binds listeners, builds Raft nodes, serves RPC
//	tc.start()                   // launches election timers
//	...                          // exercise the cluster
//	tc.shutdown()                 // Shutdown() each node + GracefulStop servers
//
// t.TempDir() is used per-node for persistence so tests never pollute ./data.
type testCluster struct {
	t *testing.T
	n int

	mu        sync.Mutex
	rafts     []*Raft
	servers   []*grpc.Server
	listeners []net.Listener
	applyChs  []chan ApplyMsg
	dataDirs  []string

	// started tracks whether Start() has been called so shutdown() can skip
	// calling Shutdown() on never-started nodes (which would hang on wg).
	started bool
}

// newTestCluster binds n ephemeral listeners, constructs n Raft nodes (each
// knowing the full id->address map including itself), and starts serving RPC.
// It does NOT call Raft.Start() — call start() to launch election timers.
func newTestCluster(t *testing.T, n int) *testCluster {
	t.Helper()
	require.GreaterOrEqual(t, n, 1, "cluster must have at least one node")

	ctx := context.Background()
	tc := &testCluster{
		t:         t,
		n:         n,
		rafts:     make([]*Raft, n),
		servers:   make([]*grpc.Server, n),
		listeners: make([]net.Listener, n),
		applyChs:  make([]chan ApplyMsg, n),
		dataDirs:  make([]string, n),
	}

	// 1. Bind ephemeral listeners and learn actual addresses.
	addrs := make(map[int]string, n)
	for i := 0; i < n; i++ {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err, "bind listener for node %d", i)
		tc.listeners[i] = lis
		addrs[i] = lis.Addr().String()
	}

	// 2. Construct Raft nodes using explicit id->address map. No auto-connect:
	// peers are dialed lazily via ensureConnected during elections, avoiding
	// the ConnectPeers Term:0 verification-RPC noise (TODO #27).
	for i := 0; i < n; i++ {
		dataDir := t.TempDir()
		tc.dataDirs[i] = dataDir
		rf, err := NewRaftWithPeers(i, addrs, dataDir, ctx)
		require.NoError(t, err, "NewRaftWithPeers node %d", i)
		tc.rafts[i] = rf
		tc.applyChs[i] = rf.GetApplyCh()
	}

	// 3. Register Raft service and serve on each listener.
	for i := 0; i < n; i++ {
		srv := grpc.NewServer()
		pb.RegisterRaftServer(srv, tc.rafts[i])
		tc.servers[i] = srv
		go func(i int) {
			_ = tc.servers[i].Serve(tc.listeners[i])
		}(i)
	}

	return tc
}

// start launches the election timer on every node. Idempotent.
func (tc *testCluster) start() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.started {
		return
	}
	for _, rf := range tc.rafts {
		rf.Start()
	}
	tc.started = true
}

// shutdown stops all Raft nodes (waiting for background goroutines to exit)
// and gracefully stops the gRPC servers. Safe to call multiple times.
func (tc *testCluster) shutdown() {
	tc.mu.Lock()
	started := tc.started
	tc.mu.Unlock()

	for _, rf := range tc.rafts {
		// Shutdown is idempotent and safe even if Start() was never called,
		// but only the apply goroutine was tracked in that case (started in
		// the constructor); wg.Wait() will still return promptly.
		rf.Shutdown()
	}
	for i, srv := range tc.servers {
		if srv != nil {
			srv.GracefulStop()
			tc.servers[i] = nil
		}
	}
	_ = started
}

// leader returns the first node currently reporting IsLeader(), or nil if no
// leader exists. Note: during transient split-vote or while a deposed leader
// has not yet stepped down, more than one node may momentarily report true.
func (tc *testCluster) leader() *Raft {
	for _, rf := range tc.rafts {
		if rf.IsLeader() {
			return rf
		}
	}
	return nil
}

// leaderId returns the serverId of the current leader, or -1 if none.
func (tc *testCluster) leaderId() int {
	for _, rf := range tc.rafts {
		if rf.IsLeader() {
			return rf.GetServerId()
		}
	}
	return -1
}

// leaderCount returns how many nodes currently report being leader. A healthy
// steady-state cluster should report exactly 1.
func (tc *testCluster) leaderCount() int {
	count := 0
	for _, rf := range tc.rafts {
		if rf.IsLeader() {
			count++
		}
	}
	return count
}

// requireLeader waits until exactly one node reports IsLeader() and returns it.
// Fails the test on timeout or if multiple leaders persist (split brain).
func (tc *testCluster) requireLeader(timeout time.Duration) *Raft {
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

// requireStableLeader waits for exactly one leader and then asserts it remains
// the sole leader for at least stableFor (no flip, no split brain). Heartbeats
// every 50ms keep followers from timing out; 200ms of stability is well above
// one heartbeat round on loopback.
func (tc *testCluster) requireStableLeader(timeout, stableFor time.Duration) *Raft {
	tc.t.Helper()
	leader := tc.requireLeader(timeout)

	require.Neverf(tc.t, func() bool {
		count := 0
		cur := -1
		for _, rf := range tc.rafts {
			if rf.IsLeader() {
				cur = rf.GetServerId()
				count++
			}
		}
		// Violation if leader changed, count drifted, or leader id mismatch.
		return count != 1 || cur != leader.GetServerId()
	}, stableFor, 10*time.Millisecond,
		"leader was not stable for %v after election", stableFor)
	return leader
}

// waitForCommit blocks until rf.commitIndex >= index or timeout elapses.
// Whitebox: reads commitIndex under r.mu.
func (tc *testCluster) waitForCommit(rf *Raft, index int, timeout time.Duration) bool {
	tc.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rf.mu.Lock()
		ok := rf.commitIndex >= index
		rf.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// requireCommit is the asserting variant of waitForCommit.
func (tc *testCluster) requireCommit(rf *Raft, index int, timeout time.Duration) {
	tc.t.Helper()
	if !tc.waitForCommit(rf, index, timeout) {
		tc.t.Fatalf("node %d: commitIndex did not reach %d within %v", rf.GetServerId(), index, timeout)
	}
}
