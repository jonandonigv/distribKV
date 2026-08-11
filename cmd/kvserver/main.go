// Command kvserver runs a single distribKV node. Flags:
//
//	-config    path to cluster.yaml (default configs/cluster.yaml)
//	-id        this node's id from the config (required)
//	-log.level debug|info|warn|error (default info)
//	-log.format text|json (default text)
//
// Startup/shutdown ordering lives in package server (see AGENTS.md
// "Lifecycle invariants"); main just wires flags to server.Options and
// blocks on SIGINT/SIGTERM.
//
// The `healthcheck` subcommand is added in step 6.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonandonigv/distribKV/server"
)

func main() {
	cfgPath := flag.String("config", "configs/cluster.yaml", "path to cluster.yaml")
	id := flag.Int("id", 0, "this node's id from cluster.yaml (required)")
	logLevel := flag.String("log.level", "info", "log level: debug|info|warn|error")
	logFormat := flag.String("log.format", "text", "log format: text|json")
	flag.Parse()

	if *id == 0 {
		slog.Error("-id is required and must be non-zero")
		os.Exit(2)
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
		os.Exit(1)
	}

	slog.Info("kvserver ready", "id", *id, "addr", node.Addr())

	// Block until a signal arrives or the gRPC server exits on its own.
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-node.ServeErr():
		if err != nil {
			slog.Error("grpc server exited", "err", err)
			os.Exit(1)
		}
	}

	node.Shutdown()
	slog.Info("kvserver stopped")
}
