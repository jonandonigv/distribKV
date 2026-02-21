package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	"github.com/jonandonigv/distribKV/pkg/kvserver"
	"github.com/jonandonigv/distribKV/pkg/raft"
	pb "github.com/jonandonigv/distribKV/proto/kv"
	raftpb "github.com/jonandonigv/distribKV/proto/raft"
)

func main() {
	// Parse flags
	var (
		id      = flag.Int("id", 0, "Server ID (0 = derive from port)")
		peers   = flag.String("peers", "", "Comma-separated peer addresses (required)")
		port    = flag.Int("port", 0, "Port to listen on (0 = 10000 + id)")
		verbose = flag.Bool("verbose", false, "Enable verbose logging")
		dataDir = flag.String("data", "./data", "Data directory for persistence")
	)
	flag.Parse()

	// Validate and set defaults
	if *peers == "" {
		log.Fatal("Must specify -peers")
	}

	if *port == 0 && *id == 0 {
		log.Fatal("Must specify either -id or -port")
	}

	if *port == 0 {
		*port = 10000 + *id
	}

	if *id == 0 {
		*id = *port % 10000
	}

	peerList := strings.Split(*peers, ",")
	addr := fmt.Sprintf("localhost:%d", *port)

	if *verbose {
		log.Printf("Starting KV Server %d on %s", *id, addr)
		log.Printf("Peers: %v", peerList)
		log.Printf("Data directory: %s", *dataDir)
	}

	// Create context for initial connections
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Initialize Raft
	rf, err := raft.NewRaft(*id, peerList, *dataDir, ctx)
	if err != nil {
		log.Fatalf("Failed to create Raft node: %v", err)
	}

	// Initialize KVServer
	kv := kvserver.NewKVServer(rf, 1000) // maxPendingOps = 1000

	// Start gRPC server
	grpcServer := common.NewServer(addr)
	if err := grpcServer.Start(); err != nil {
		log.Fatalf("Failed to start gRPC server: %v", err)
	}

	// Register KV service (for clients)
	grpcServer.RegisterService(&pb.KV_ServiceDesc, kv)

	// Register Raft service (for peer-to-peer consensus)
	grpcServer.RegisterService(&raftpb.Raft_ServiceDesc, rf)

	// Start Raft election process
	rf.Start()

	log.Printf("KV Server %d ready", *id)

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")

	// Graceful shutdown
	grpcServer.Stop()
	kv.Kill()

	log.Println("Shutdown complete")
}
