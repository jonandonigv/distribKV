// Command kvserver is the single distribKV binary. It has two modes:
//
//  1. server (default): run a node. Flags:
//     -config    path to cluster.yaml (default configs/cluster.yaml)
//     -id        this node's id from the config (required)
//     -log.level debug|info|warn|error (default info)
//     -log.format text|json (default text)
//
//  2. healthcheck subcommand: dial the node's own listen_addr (looked up
//     from the config by -id) and call Health.Check with a short timeout.
//     Prints "SERVING" and exits 0 on success; exits 1 on any failure.
//     Used by docker-compose healthcheck blocks and the `make smoke`
//     readiness check. Flags: -config, -id.
//
// Startup/shutdown ordering lives in package server (see AGENTS.md
// "Lifecycle invariants"); main just wires flags to server.Options.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonandonigv/distribKV/config"
	"github.com/jonandonigv/distribKV/health"
	"github.com/jonandonigv/distribKV/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	// Subcommand dispatch: `kvserver healthcheck ...` runs the healthcheck
	// mode; everything else is the server.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthCheck(os.Args[2:]))
	}

	os.Exit(runServer())
}

// runServer parses the server flags, starts the node, and blocks on
// SIGINT/SIGTERM or a premature gRPC Serve failure.
func runServer() int {
	cfgPath := flag.String("config", "configs/cluster.yaml", "path to cluster.yaml")
	id := flag.Int("id", 0, "this node's id from cluster.yaml (required)")
	logLevel := flag.String("log.level", "info", "log level: debug|info|warn|error")
	logFormat := flag.String("log.format", "text", "log format: text|json")
	flag.Parse()

	if *id == 0 {
		slog.Error("-id is required and must be non-zero")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	node, err := server.Start(server.Options{
		ConfigPath: *cfgPath,
		NodeID:     *id,
		LogLevel:   *logLevel,
		LogFormat:  *logFormat,
	})
	if err != nil {
		slog.Error("startup failed", "err", err)
		return 1
	}

	slog.Info("kvserver ready", "id", *id, "addr", node.Addr())

	// Block until a signal arrives or the gRPC server exits on its own.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-node.ServeErr():
		if err != nil {
			slog.Error("grpc server exited", "err", err)
			return 1
		}
	}

	node.Shutdown()
	slog.Info("kvserver stopped")
	return 0
}

// runHealthCheck implements the `kvserver healthcheck` subcommand. It
// loads the config, looks up the node's listen_addr by -id, dials it
// with a 2s timeout, and calls Health.Check. Prints SERVING and returns
// 0 on success, 1 on any failure. The healthcheck runs inside the node's
// own container so dialing the listen_addr (even 0.0.0.0:N) reaches the
// local server.
func runHealthCheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/cluster.yaml", "path to cluster.yaml")
	id := fs.Int("id", 0, "node id to check (required)")
	_ = fs.Parse(args)

	if *id == 0 {
		fmt.Fprintln(os.Stderr, "healthcheck: -id is required and must be non-zero")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: load config: %v\n", err)
		return 1
	}
	node, err := cfg.NodeByID(*id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(node.ListenAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: dial %s: %v\n", node.ListenAddr, err)
		return 1
	}
	defer conn.Close()

	resp, err := health.NewHealthClient(conn).Check(ctx, &health.HealthCheckRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	fmt.Println(resp.Status)
	return 0
}
