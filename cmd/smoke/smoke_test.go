// Integration test for the smoke tool: starts a real 1-node cluster via
// the production server wiring and runs smoke against it. Validates that
// the Put->Append->Get sequence succeeds end-to-end through the Clerk.
//
// To avoid port-reservation races, the node is started on an ephemeral
// port (config :0); once bound, a second config holding the actual
// address is written for smoke to read. No restart.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/server"
	"github.com/stretchr/testify/require"
)

// writeClusterYAML writes a one-node cluster.yaml with the given listen
// address and data dir at path.
func writeClusterYAML(t *testing.T, path, listenAddr, dataDir string) {
	t.Helper()
	content := `cluster:
  name: distribkv-smoke
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: ` + dataDir + `
  snapshot_threshold: 0
nodes:
  - id: 1
    listen_addr: "` + listenAddr + `"
`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}

func TestRun_SucceedsAgainstLiveNode(t *testing.T) {
	dataDir := t.TempDir()
	dir := t.TempDir()
	startCfg := filepath.Join(dir, "start.yaml")
	smokeCfg := filepath.Join(dir, "smoke.yaml")

	// Start the node on an ephemeral port.
	writeClusterYAML(t, startCfg, "127.0.0.1:0", dataDir)
	node, err := server.Start(server.Options{
		ConfigPath: startCfg,
		NodeID:     1,
		LogLevel:   "info",
		LogFormat:  "text",
	})
	require.NoError(t, err)
	defer node.Shutdown()

	// Wait for the single node to self-elect before exercising it.
	require.Eventually(t, func() bool { return node.Raft().IsLeader() },
		5*time.Second, 10*time.Millisecond, "node did not self-elect")

	// Smoke reads the node addresses from its own config; write one with
	// the actually-bound address. Single-node => no peers, so only the
	// listen_addr matters and the raft peer map (empty) is unaffected.
	writeClusterYAML(t, smokeCfg, node.Addr(), dataDir)

	err = run(smokeCfg, 20*time.Second)
	require.NoError(t, err, "smoke run failed against live node at %s", node.Addr())
}

func TestRun_MissingConfig(t *testing.T) {
	err := run(filepath.Join(t.TempDir(), "nope.yaml"), 2*time.Second)
	require.Error(t, err)
}
