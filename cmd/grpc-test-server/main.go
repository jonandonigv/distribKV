package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonandonigv/distribKV/pkg/common"
	"github.com/jonandonigv/distribKV/pkg/health"
	healthpb "github.com/jonandonigv/distribKV/proto"
)

func main() {
	port := flag.Int("port", 50051, "Server port")
	serverID := flag.String("id", "1", "Server identifier for logging")
	flag.Parse()

	log.SetPrefix(fmt.Sprintf("[%s] ", *serverID))

	server := common.NewServer(fmt.Sprintf(":%d", *port))
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	healthSrv := health.NewServer()
	server.RegisterService(&healthpb.Health_ServiceDesc, healthSrv)

	log.Printf("Health check server listening on port %d", *port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down server...")
	server.Stop()
	log.Println("Server stopped gracefully")
}
