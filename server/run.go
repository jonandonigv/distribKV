// Server lifecycle: load config, build the logger, bind the listener,
// construct the persister, raft, and kv state machine, register the
// gRPC services, and drive start/serve/shutdown.
//
// Startup order (AGENTS.md "Lifecycle invariants"):
//
//	load config -> find node by id -> build logger -> bind listener
//	-> create persister -> raft.NewRaft -> kv.NewServer
//	-> register services -> grpcServer.Serve() (goroutine) -> rf.Start()
//
// Shutdown order (AGENTS.md):
//
//	rf.Shutdown() -> kv.Kill() -> grpcServer.GracefulStop()
//
// Never dial peers before the local server is listening: the listener
// is bound BEFORE raft.NewRaft is even called, and raft only dials lazily
// on the first failed RPC (see raft.ensureConnected).

package server

import (
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/jonandonigv/distribKV/config"
	"github.com/jonandonigv/distribKV/health"
	"github.com/jonandonigv/distribKV/kv"
	"github.com/jonandonigv/distribKV/raft"
	raftpb "github.com/jonandonigv/distribKV/raft/raftpb"
	"google.golang.org/grpc"
)

// Options configures a production node. Fields map to the kvserver flags.
type Options struct {
	ConfigPath string // -config; required
	NodeID     int    // -id; required, must exist in the config
	LogLevel   string // -log.level; debug|info|warn|error (default info)
	LogFormat  string // -log.format; text|json (default text)
}

// Node is a running production node: the gRPC server, raft, and KV state
// machine, plus the bound listener. Returned by Start; stopped by Shutdown.
type Node struct {
	cfg      *config.Config
	node     *config.Node
	logger   *slog.Logger
	listener net.Listener
	grpc     *grpc.Server
	rf       *raft.Raft
	kvs      *kv.Server
	serveErr chan error // receives grpcServer.Serve's return value
}

// Start loads Options.ConfigPath, validates the node id, builds the logger,
// and brings the node up per AGENTS.md startup order. Returns the running
// Node; call Shutdown to stop it.
func Start(opts Options) (*Node, error) {
	if opts.ConfigPath == "" {
		return nil, fmt.Errorf("config path is required")
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	node, err := cfg.NodeByID(opts.NodeID)
	if err != nil {
		return nil, err
	}
	logger, err := buildLogger(opts.LogLevel, opts.LogFormat)
	if err != nil {
		return nil, err
	}
	return StartFromConfig(cfg, node.ID, logger)
}

// StartFromConfig builds a node from an already-loaded config. Exposed
// for tests that construct the config in memory (e.g. with temporary
// data directories) and don't want to round-trip through a YAML file.
// The logger must be non-nil; nodeID must exist in cfg.
func StartFromConfig(cfg *config.Config, nodeID int, logger *slog.Logger) (*Node, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	node, err := cfg.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	logger = logger.With(slog.Int("node", node.ID))

	// 1. Bind listener BEFORE constructing raft (never dial peers before
	//    the local server is listening).
	lis, err := net.Listen("tcp", node.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", node.ListenAddr, err)
	}

	// 2. File-backed persister at <data_dir>/raft-state.json.
	statePath := filepath.Join(node.DataDir, "raft-state.json")
	persister := raft.NewFilePersister(statePath)

	// 3. Build the peer map (every other node).
	peers := make(map[int]string)
	for _, p := range cfg.PeersOf(node.ID) {
		peers[p.ID] = p.ListenAddr
	}

	// 4. Construct raft WITHOUT starting goroutines (Start does that).
	rf, err := raft.NewRaft(raft.Config{
		ServerID:           node.ID,
		OwnAddr:            node.ListenAddr,
		Peers:              peers,
		ElectionTimeoutMin: time.Duration(cfg.Cluster.ElectionTimeoutMin),
		ElectionTimeoutMax: time.Duration(cfg.Cluster.ElectionTimeoutMax),
		HeartbeatInterval:  time.Duration(cfg.Cluster.HeartbeatInterval),
		SnapshotThreshold:  cfg.Cluster.SnapshotThreshold,
		Persister:          persister,
		Logger:             logger,
	})
	if err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("construct raft: %w", err)
	}

	// 5. Construct the KV state machine (starts its apply loop).
	kvs := kv.NewServer(rf, kv.DefaultMaxPendingOps, logger)

	// 6. Register raft + kv + health services on a keepalive-configured
	//    server. Health is registered here (Check returns SERVING once
	//    the gRPC server accepts connections); see AGENTS.md.
	grpcSrv := newGRPCServer()
	raftpb.RegisterRaftServer(grpcSrv, rf)
	kv.RegisterKVServer(grpcSrv, kvs)
	health.RegisterHealthServer(grpcSrv, health.NewServer())

	// 7. Serve in a goroutine; capture the serve error so Shutdown can
	//    detect a premature listener failure.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcSrv.Serve(lis)
	}()

	// 8. Start raft election timer / apply loop AFTER the server is
	//    listening (AGENTS.md lifecycle invariant).
	rf.Start()

	return &Node{
		cfg:      cfg,
		node:     node,
		logger:   logger,
		listener: lis,
		grpc:     grpcSrv,
		rf:       rf,
		kvs:      kvs,
		serveErr: serveErr,
	}, nil
}

// Addr returns the bound address of the node's gRPC listener. Useful for
// tests (ephemeral ports) and smoke tooling.
func (n *Node) Addr() string { return n.listener.Addr().String() }

// ServeErr returns a channel that receives the gRPC server's return
// value from Serve. It fires once when the server stops (either because
// Shutdown was called or because the listener failed). Callers (e.g.
// main) select on this to detect a premature listener failure.
func (n *Node) ServeErr() <-chan error { return n.serveErr }

// Raft returns the underlying raft node. Used by tests that want to
// assert raft state through the production wiring.
func (n *Node) Raft() *raft.Raft { return n.rf }

// KV returns the underlying KV server.
func (n *Node) KV() *kv.Server { return n.kvs }

// Shutdown stops the node in the AGENTS.md shutdown order:
// raft -> kv -> grpc. Idempotent in the sense that each layer's own
// shutdown is idempotent.
func (n *Node) Shutdown() {
	n.rf.Shutdown()
	n.kvs.Kill()
	n.grpc.GracefulStop()
}
