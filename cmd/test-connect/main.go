package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/jonandonigv/distribKV/pkg/raft"
)

func main() {
	serverId := flag.Int("id", 1, "Server ID")
	flag.Parse()

	// Test configuration: 3-node cluster
	peerAddresses := []string{
		"localhost:10001",
		"localhost:10002",
		"localhost:10003",
	}

	log.Printf("Creating Raft node %d with peers: %v", *serverId, peerAddresses)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create Raft node (will try to connect to all peers)
	r, err := raft.NewRaft(*serverId, peerAddresses, ctx)
	if err != nil {
		log.Fatalf("Failed to create Raft node: %v", err)
	}

	log.Printf("Raft node created successfully!")
	log.Printf("Server ID: %d", r.GetServerId())
	log.Printf("Address: %s", r.GetAddress())
	log.Printf("Peer count: %d", r.GetPeerCount())

	fmt.Println("\nConnection test completed successfully!")
}
