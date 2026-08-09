package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig is a tiny helper that drops the given yaml content into a temp
// file and returns its path. Keeps tests table-driven without touching the
// canonical configs/cluster.yaml on disk.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

// canonical is the cluster.yaml shape we commit to; load tests use it as the
// baseline "happy path".
const canonical = `
cluster:
  name: distribkv-dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
  snapshot_threshold: 100
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
  - id: 2
    listen_addr: "0.0.0.0:10002"
    data_dir: /var/lib/distribkv/2
  - id: 3
    listen_addr: "0.0.0.0:10003"
`

func TestLoad_HappyPath(t *testing.T) {
	path := writeConfig(t, canonical)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Cluster-level fields parse with full duration fidelity.
	assert.Equal(t, "distribkv-dev", cfg.Cluster.Name)
	assert.Equal(t, 50*time.Millisecond, time.Duration(cfg.Cluster.HeartbeatInterval))
	assert.Equal(t, 150*time.Millisecond, time.Duration(cfg.Cluster.ElectionTimeoutMin))
	assert.Equal(t, 300*time.Millisecond, time.Duration(cfg.Cluster.ElectionTimeoutMax))
	assert.Equal(t, "/var/lib/distribkv", cfg.Cluster.DataDir)
	assert.Equal(t, 100, cfg.Cluster.SnapshotThreshold)

	// Three nodes parsed.
	require.Len(t, cfg.Nodes, 3)

	// Node 1: no explicit data_dir -> defaults to <cluster.data_dir>/<id>.
	n1, err := cfg.NodeByID(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n1.ID)
	assert.Equal(t, "0.0.0.0:10001", n1.ListenAddr)
	assert.Equal(t, "/var/lib/distribkv/1", n1.DataDir, "empty data_dir should default to <base>/<id>")

	// Node 2: explicit data_dir preserved.
	n2, err := cfg.NodeByID(2)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/distribkv/2", n2.DataDir, "explicit data_dir should be preserved")

	// Node 3.
	n3, err := cfg.NodeByID(3)
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:10003", n3.ListenAddr)
	assert.Equal(t, "/var/lib/distribkv/3", n3.DataDir)
}

func TestLoad_DefaultSnapshotThreshold(t *testing.T) {
	// Omit snapshot_threshold entirely; should default to 0 (disabled).
	const yamlStr = `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
`
	cfg, err := config.Load(writeConfig(t, yamlStr))
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.Cluster.SnapshotThreshold)
}

func TestLoad_PeersOf(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, canonical))
	require.NoError(t, err)

	// PeersOf(1) returns nodes 2 and 3 (not necessarily ordered).
	peers := cfg.PeersOf(1)
	require.Len(t, peers, 2)
	ids := map[int]bool{}
	for _, p := range peers {
		ids[p.ID] = true
	}
	assert.True(t, ids[2] && ids[3], "peers of 1 should be 2 and 3, got %v", ids)

	// PeersOf for the only other node returns the remaining set.
	require.Len(t, cfg.PeersOf(2), 2)
}

func TestNodeByID_NotFound(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, canonical))
	require.NoError(t, err)

	_, err = cfg.NodeByID(999)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoad_Errors(t *testing.T) {
	tests := []struct {
		name    string
		yamlStr string
		wantErr string
	}{
		{
			name: "no nodes",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
nodes: []
`,
			wantErr: "no nodes",
		},
		{
			name: "duplicate ids",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
  - id: 1
    listen_addr: "0.0.0.0:10002"
`,
			wantErr: "duplicate node id 1",
		},
		{
			name: "missing listen_addr",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
nodes:
  - id: 1
`,
			wantErr: "listen_addr is required",
		},
		{
			name: "election min >= max",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 300ms
  election_timeout_max: 150ms
  data_dir: /var/lib/distribkv
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
`,
			wantErr: "election_timeout_min must be less than",
		},
		{
			name: "missing data_dir",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
`,
			wantErr: "data_dir is required",
		},
		{
			name: "invalid duration",
			yamlStr: `
cluster:
  name: dev
  heartbeat_interval: not-a-duration
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: /var/lib/distribkv
nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
`,
			wantErr: "invalid duration",
		},
		{
			name:    "corrupt yaml",
			yamlStr: `this: is: not: valid: yaml`,
			wantErr: "parse config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.yamlStr))
			require.Error(t, err, "expected error containing %q", tt.wantErr)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config")
}
