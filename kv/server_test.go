package kv_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/kv"
	"github.com/jonandonigv/distribKV/raft"
	raftpb "github.com/jonandonigv/distribKV/raft/raftpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// logWriter routes slog through t.Log for clean test attribution.
type logWriter struct{ t *testing.T }

func (w *logWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// kvCluster spins a 3-node Raft + KV cluster on ephemeral ports.
type kvCluster struct {
	t        *testing.T
	n        int
	rafts    []*raft.Raft
	servers  []*kv.Server
	grpcSrvs []*grpc.Server
	addrs    []string
}

func newKVCluster(t *testing.T, n int) *kvCluster {
	t.Helper()
	require.GreaterOrEqual(t, n, 1)

	// Bind ephemeral listeners.
	listeners := make([]net.Listener, n)
	addrs := make([]string, n)
	for i := 0; i < n; i++ {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listeners[i] = lis
		addrs[i] = lis.Addr().String()
	}

	// Build peer maps.
	c := &kvCluster{
		t:        t,
		n:        n,
		rafts:    make([]*raft.Raft, n),
		servers:  make([]*kv.Server, n),
		grpcSrvs: make([]*grpc.Server, n),
		addrs:    addrs,
	}

	logH := slog.New(slog.NewTextHandler(&logWriter{t: t}, nil))

	for i := 0; i < n; i++ {
		peers := make(map[int]string)
		for j := 0; j < n; j++ {
			if j != i {
				peers[j] = addrs[j]
			}
		}
		rf, err := raft.NewRaft(raft.Config{
			ServerID:           i,
			OwnAddr:            addrs[i],
			Peers:              peers,
			ElectionTimeoutMin: 150 * time.Millisecond,
			ElectionTimeoutMax: 300 * time.Millisecond,
			HeartbeatInterval:  50 * time.Millisecond,
			Persister:          raft.NewMemoryPersister(),
			Logger:             logH,
		})
		require.NoError(t, err)
		c.rafts[i] = rf
		c.servers[i] = kv.NewServer(rf, kv.DefaultMaxPendingOps, logH)
	}

	// Register both Raft + KV services on each gRPC server.
	for i := 0; i < n; i++ {
		srv := grpc.NewServer()
		raftpb.RegisterRaftServer(srv, c.rafts[i])
		kv.RegisterKVServer(srv, c.servers[i])
		c.grpcSrvs[i] = srv
		go func(i int) { _ = c.grpcSrvs[i].Serve(listeners[i]) }(i)
	}

	// Start raft election timers (must be after gRPC servers are listening).
	for _, rf := range c.rafts {
		rf.Start()
	}

	return c
}

func (c *kvCluster) shutdown() {
	c.t.Helper()
	for _, rf := range c.rafts {
		rf.Shutdown()
	}
	for _, s := range c.servers {
		s.Kill()
	}
	for _, srv := range c.grpcSrvs {
		srv.GracefulStop()
	}
}

func (c *kvCluster) waitLeader(timeout time.Duration) int {
	c.t.Helper()
	var leaderId int = -1
	require.Eventuallyf(c.t, func() bool {
		count := 0
		for _, rf := range c.rafts {
			if rf.IsLeader() {
				leaderId = rf.GetServerId()
				count++
			}
		}
		return count == 1
	}, timeout, 10*time.Millisecond, "expected exactly one leader within %v", timeout)
	return leaderId
}

// ---------------------------------------------------------------------------
// End-to-end integration tests.
// ---------------------------------------------------------------------------

func TestKV_BasicPutGet(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	ck.Put("foo", "bar")
	assert.Equal(t, "bar", ck.Get("foo"))
}

func TestKV_Append(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	ck.Put("key", "hello")
	ck.Append("key", " ")
	ck.Append("key", "world")
	assert.Equal(t, "hello world", ck.Get("key"))
}

func TestKV_GetMissingKey(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	// Get on a non-existent key returns "" (not an error from the
	// Clerk's perspective — the server returns success=false with
	// error=key not found, but Clerk.Get returns the value which is "").
	result := ck.Get("nonexistent")
	assert.Empty(t, result)
}

func TestKV_Overwrite(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	ck.Put("k", "v1")
	ck.Put("k", "v2")
	assert.Equal(t, "v2", ck.Get("k"))
}

func TestKV_MultipleKeys(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	ck.Put("a", "1")
	ck.Put("b", "2")
	ck.Put("c", "3")
	assert.Equal(t, "1", ck.Get("a"))
	assert.Equal(t, "2", ck.Get("b"))
	assert.Equal(t, "3", ck.Get("c"))
}

func TestKV_WorksAfterLeaderChange(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	leaderId := c.waitLeader(5 * time.Second)

	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()

	// Write before leadership change.
	ck.Put("before", "first")

	// Shutdown the leader's raft (not the whole server, just the
	// consensus engine — KV servers keep serving RPCs, just now as
	// followers).
	c.rafts[leaderId].Shutdown()

	// Wait for a new leader.
	require.Eventuallyf(t, func() bool {
		count := 0
		newId := -1
		for i, rf := range c.rafts {
			if rf.IsLeader() {
				newId = i
				count++
			}
		}
		return count == 1 && newId != leaderId
	}, 5*time.Second, 10*time.Millisecond,
		"expected new leader after old leader shutdown")

	// Write after leadership change and verify the old data is still there
	// (persisted on the two remaining nodes via raft log replication).
	ck.Put("after", "second")
	assert.Equal(t, "first", ck.Get("before"))
	assert.Equal(t, "second", ck.Get("after"))
}

func TestKV_ConcurrentClerks(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	c.waitLeader(5 * time.Second)

	// Multiple clerks writing different keys concurrently.
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		i := i
		go func() {
			ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
			defer ck.CloseConn()
			key := fmt.Sprintf("ck-key-%d", i)
			val := fmt.Sprintf("ck-val-%d", i)
			ck.Put(key, val)
			got := ck.Get(key)
			if got != val {
				done <- fmt.Errorf("clerk %d: got %q expected %q", i, got, val)
				return
			}
			done <- nil
		}()
	}

	for i := 0; i < 5; i++ {
		select {
		case err := <-done:
			require.NoError(t, err, "concurrent clerk failed")
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent clerk did not complete within 10s")
		}
	}

	// Verify all keys were written.
	ck := kv.MakeClerk(c.addrs, []int{0, 1, 2}, nil)
	defer ck.CloseConn()
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("ck-key-%d", i)
		val := fmt.Sprintf("ck-val-%d", i)
		assert.Equal(t, val, ck.Get(key), "key %s should survive", key)
	}
}

func TestKV_FollowerRejectsWrite(t *testing.T) {
	c := newKVCluster(t, 3)
	defer c.shutdown()
	leaderId := c.waitLeader(5 * time.Second)

	// Find a follower.
	var followerIdx int
	for i := range c.rafts {
		if i != leaderId {
			followerIdx = i
			break
		}
	}

	// Connect directly to the follower and try a Put. Should get
	// wrong_leader=true.
	ctx, cancel := context.WithTimeout(context.Background(), kv.RPCTimeout)
	defer cancel()
	conn, err := grpc.NewClient(c.addrs[followerIdx],
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer conn.Close()

	resp, err := kv.NewKVClient(conn).Put(ctx, &kv.PutRequest{Key: "x", Value: "y"})
	require.NoError(t, err)
	assert.True(t, resp.GetWrongLeader(), "follower must return wrong_leader=true")
	assert.Equal(t, int32(leaderId), resp.GetLeaderId(), "leader_id hint should point to the real leader")
}
