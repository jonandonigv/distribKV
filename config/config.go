// Package config parses the cluster.yaml definition that drives every
// distribKV node. The schema is documented in configs/cluster.yaml and
// AGENTS.md; load-bearing notes:
//
//   - Node IDs are opaque integers from yaml — never derived from addresses.
//   - Durations are written as strings ("50ms", "150ms") and parsed with
//     time.ParseDuration via the custom Duration type below.
//   - Per-node data_dir is optional; empty values resolve to
//     <cluster.data_dir>/<id> so every node ends up with a concrete path.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the parsed cluster definition.
type Config struct {
	Cluster Cluster `yaml:"cluster"`
	Nodes   []Node  `yaml:"nodes"`
}

// Cluster holds cluster-wide tunables.
type Cluster struct {
	Name               string   `yaml:"name"`
	HeartbeatInterval  Duration `yaml:"heartbeat_interval"`
	ElectionTimeoutMin Duration `yaml:"election_timeout_min"`
	ElectionTimeoutMax Duration `yaml:"election_timeout_max"`
	DataDir            string   `yaml:"data_dir"`
	SnapshotThreshold  int      `yaml:"snapshot_threshold"`
}

// Node is one entry in the cluster's nodes list.
type Node struct {
	ID         int    `yaml:"id"`
	ListenAddr string `yaml:"listen_addr"`
	DataDir    string `yaml:"data_dir"` // optional override; resolved if empty
}

// Duration wraps time.Duration so yaml values like "50ms" parse cleanly.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler. It accepts the same duration
// strings as time.ParseDuration ("50ms", "1s", "150ms", ...).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Load reads, parses, and validates the cluster.yaml at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	cfg.resolveDataDirs()
	return &cfg, nil
}

// validate enforces the schema invariants documented in AGENTS.md.
func (c *Config) validate() error {
	if len(c.Nodes) == 0 {
		return errors.New("no nodes defined")
	}
	if c.Cluster.HeartbeatInterval <= 0 {
		return errors.New("heartbeat_interval must be positive")
	}
	if c.Cluster.ElectionTimeoutMin <= 0 || c.Cluster.ElectionTimeoutMax <= 0 {
		return errors.New("election timeouts must be positive")
	}
	if c.Cluster.ElectionTimeoutMin >= c.Cluster.ElectionTimeoutMax {
		return fmt.Errorf("election_timeout_min must be less than election_timeout_max (got %s >= %s)",
			time.Duration(c.Cluster.ElectionTimeoutMin), time.Duration(c.Cluster.ElectionTimeoutMax))
	}
	if c.Cluster.DataDir == "" {
		return errors.New("cluster.data_dir is required")
	}

	seen := make(map[int]bool)
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.ID < 0 {
			return fmt.Errorf("node id %d must be non-negative", n.ID)
		}
		if n.ListenAddr == "" {
			return fmt.Errorf("node %d: listen_addr is required", n.ID)
		}
		if seen[n.ID] {
			return fmt.Errorf("duplicate node id %d", n.ID)
		}
		seen[n.ID] = true
	}
	return nil
}

// resolveDataDirs fills in any empty Node.DataDir with
// <cluster.data_dir>/<id> so callers can always rely on a concrete path.
func (c *Config) resolveDataDirs() {
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.DataDir == "" {
			n.DataDir = filepath.Join(c.Cluster.DataDir, strconv.Itoa(n.ID))
		}
	}
}

// NodeByID returns the node with the given id, or an error if not found.
// Used by the binary to locate its own entry after parsing -id.
func (c *Config) NodeByID(id int) (*Node, error) {
	for i := range c.Nodes {
		if c.Nodes[i].ID == id {
			return &c.Nodes[i], nil
		}
	}
	return nil, fmt.Errorf("node id %d not found in config", id)
}

// PeersOf returns every node except the one with the given id. The result
// is not sorted; callers that need stability should sort it themselves.
func (c *Config) PeersOf(id int) []*Node {
	peers := make([]*Node, 0, len(c.Nodes)-1)
	for i := range c.Nodes {
		if c.Nodes[i].ID != id {
			peers = append(peers, &c.Nodes[i])
		}
	}
	return peers
}
