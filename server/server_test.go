// Blackbox integration tests for the production server wiring. Covers
// the Start lifecycle (config -> listener -> persister -> raft -> kv ->
// gRPC), a single-node Put/Get through the real binary path, persistence
// across restart, and the error paths. Uses the real gRPC stack with
// ephemeral listeners and a Clerk client (end-to-end through Raft).
//
// Per AGENTS.md: t.TempDir() for any disk state (never ./data), race-safe
// require.Eventually for async, testify require/assert.

package server_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/health"
	"github.com/jonandonigv/distribKV/kv"
	"github.com/jonandonigv/distribKV/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// configYAML builds a one-node cluster.yaml with the given listen_addr
// and data_dir. The node id is 1.
func configYAML(listenAddr, dataDir string) string {
	return `cluster:
  name: distribkv-test
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: ` + dataDir + `
  snapshot_threshold: 0
nodes:
  - id: 1
    listen_addr: "` + listenAddr + `"
`
}

// writeFile writes content to path, creating parent dirs.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}

// startSingleNode loads a one-node config from a temp dir, starts the
// node, and returns it along with its bound address. The listener uses
// an ephemeral port (127.0.0.1:0) so tests don't collide.
func startSingleNode(t *testing.T, dataDir string) *server.Node {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "cluster.yaml")
	writeFile(t, configPath, configYAML("127.0.0.1:0", dataDir))
	node, err := server.Start(server.Options{
		ConfigPath: configPath,
		NodeID:     1,
		LogLevel:   "info",
		LogFormat:  "text",
	})
	require.NoError(t, err)
	require.NotNil(t, node)
	return node
}

// eventuallyLeader waits for the node's raft to become leader (single
// node self-elects on Start).
func eventuallyLeader(t *testing.T, node *server.Node, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		return node.Raft().IsLeader()
	}, timeout, 10*time.Millisecond, "node did not self-elect as leader")
}

func TestBuildLogger_InvalidLevel(t *testing.T) {
	_, err := server.BuildLogger("nope", "text")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log level")
}

func TestBuildLogger_InvalidFormat(t *testing.T) {
	_, err := server.BuildLogger("info", "yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log format")
}

func TestBuildLogger_Defaults(t *testing.T) {
	l, err := server.BuildLogger("", "")
	require.NoError(t, err)
	require.NotNil(t, l)
}

func TestStart_MissingConfigPath(t *testing.T) {
	_, err := server.Start(server.Options{ConfigPath: "", NodeID: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config path")
}

func TestStart_NonexistentConfigFile(t *testing.T) {
	_, err := server.Start(server.Options{ConfigPath: filepath.Join(t.TempDir(), "missing.yaml"), NodeID: 1})
	require.Error(t, err)
}

func TestStart_UnknownNodeID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cluster.yaml")
	writeFile(t, configPath, configYAML("127.0.0.1:0", t.TempDir()))
	_, err := server.Start(server.Options{ConfigPath: configPath, NodeID: 999})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStart_BadLogLevel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cluster.yaml")
	writeFile(t, configPath, configYAML("127.0.0.1:0", t.TempDir()))
	_, err := server.Start(server.Options{ConfigPath: configPath, NodeID: 1, LogLevel: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log level")
}

func TestStart_SingleNodePutGet(t *testing.T) {
	node := startSingleNode(t, t.TempDir())
	defer node.Shutdown()
	eventuallyLeader(t, node, 5*time.Second)

	addrs := []string{node.Addr()}
	ids := []int{1}
	ck := kv.MakeClerk(addrs, ids, testLogger(t))
	defer ck.CloseConn()

	ck.Put("foo", "bar")
	require.Equal(t, "bar", ck.Get("foo"))
}

func TestStart_PersistCreatesDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	node := startSingleNode(t, dataDir)
	defer node.Shutdown()
	eventuallyLeader(t, node, 5*time.Second)

	ck := kv.MakeClerk([]string{node.Addr()}, []int{1}, testLogger(t))
	defer ck.CloseConn()
	ck.Put("k", "v")

	// config.resolveDataDirs appends "/<id>", so the file lands under
	// <dataDir>/1/raft-state.json (not <dataDir> directly).
	statePath := filepath.Join(dataDir, "1", "raft-state.json")
	require.Eventually(t, func() bool {
		_, err := os.Stat(statePath)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "raft-state.json was not created")
}

func TestStart_RestartRecoversRaftAndAcceptsNewWrites(t *testing.T) {
	dataDir := t.TempDir()

	n1 := startSingleNode(t, dataDir)
	eventuallyLeader(t, n1, 5*time.Second)
	term1 := n1.Raft().CurrentTerm()
	ck1 := kv.MakeClerk([]string{n1.Addr()}, []int{1}, testLogger(t))
	ck1.Put("persisted", "yes")
	ck1.CloseConn()
	n1.Shutdown()

	// Restart with the SAME data_dir. The FilePersister restores the
	// raft log and term; the node re-elects and accepts new writes.
	//
	// NOTE: the in-memory KV map is NOT persisted in 0.1.0 (snapshotting
	// is deferred per AGENTS.md). loadPersistedState snaps lastApplied to
	// commitIndex so committed entries are not replayed — the old value
	// is therefore gone after restart. This test asserts raft recovery,
	// not KV value recovery (which is snapshot-territory).
	n2 := startSingleNode(t, dataDir)
	defer n2.Shutdown()
	eventuallyLeader(t, n2, 5*time.Second)

	// The persisted term survives the restart (raft log was durable).
	assert.GreaterOrEqual(t, n2.Raft().CurrentTerm(), term1)

	// New writes after restart succeed.
	ck2 := kv.MakeClerk([]string{n2.Addr()}, []int{1}, testLogger(t))
	defer ck2.CloseConn()
	ck2.Put("after", "restart")
	require.Equal(t, "restart", ck2.Get("after"))
}

// TestStart_HealthServiceRegistered dials the node's gRPC server with a
// Health client and calls Check, confirming the Health service is
// registered through the production wiring (server/run.go).
func TestStart_HealthServiceRegistered(t *testing.T) {
	node := startSingleNode(t, t.TempDir())
	defer node.Shutdown()

	conn, err := grpc.NewClient(node.Addr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := health.NewHealthClient(conn).Check(ctx, &health.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, health.HealthCheckResponse_SERVING, resp.GetStatus())
}

// testLogger routes slog through t.Log for clean attribution.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, nil))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
