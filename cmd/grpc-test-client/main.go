package main

// TODO: This is a test tool - NOT the production KV client
//
// Purpose: Test gRPC infrastructure using Health Check service
//
// Production Client Needed:
// - Use pkg/kvserver/clerk.go for actual KV operations
// - Connect to Raft cluster via KV service
// - Handle leader discovery and redirects
// - Implement retry logic for failures

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	healthpb "github.com/jonandonigv/distribKV/proto"
)

func main() {
	serverAddr := flag.String("addr", "localhost:50051", "Server address")
	mode := flag.String("mode", "single", "Mode: 'single' or 'continuous'")
	numReqs := flag.Int("requests", 1, "Number of requests (continuous mode)")
	delay := flag.Duration("delay", 1*time.Second, "Delay between requests (continuous mode)")
	clientID := flag.String("id", "client", "Client identifier for logging")
	flag.Parse()

	log.SetPrefix(fmt.Sprintf("[%s] ", *clientID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Received interrupt signal, cancelling...")
		cancel()
	}()

	client := common.NewClient(*serverAddr)
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()

	if err := common.WithRetry(connectCtx, 3, 500*time.Millisecond, func() error {
		return client.Connect(connectCtx)
	}); err != nil {
		log.Fatalf("Failed to connect to server: %v", err)
	}
	defer client.Close()

	log.Printf("Connected to server at %s", *serverAddr)

	healthClient := healthpb.NewHealthClient(client.Conn())

	switch *mode {
	case "single":
		runSingleCheck(ctx, healthClient)
	case "continuous":
		runContinuousCheck(ctx, healthClient, *numReqs, *delay)
	default:
		log.Fatalf("Invalid mode: %s (use 'single' or 'continuous')", *mode)
	}
}

func runSingleCheck(ctx context.Context, healthClient healthpb.HealthClient) {
	start := time.Now()
	req := &healthpb.HealthCheckRequest{}

	resp, err := healthClient.Check(ctx, req)
	if err != nil {
		log.Printf("Health check failed: %v", err)
		os.Exit(1)
	}

	latency := time.Since(start).Milliseconds()
	log.Printf("Health check status: %v (latency: %dms)", resp.Status, latency)

	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		os.Exit(1)
	}
}

func runContinuousCheck(ctx context.Context, healthClient healthpb.HealthClient, numReqs int, delay time.Duration) {
	var successCount int
	var totalLatency time.Duration

	for i := 0; i < numReqs; i++ {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, stopping...")
			goto report
		default:
		}

		start := time.Now()
		resp, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil {
			log.Printf("Request %d failed: %v", i+1, err)
		} else {
			successCount++
			latency := time.Since(start)
			totalLatency += latency
			log.Printf("Request %d: %v (%dms)", i+1, resp.Status, latency.Milliseconds())
		}

		if i < numReqs-1 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				break
			}
		}
	}

report:
	if successCount > 0 {
		avgLatency := totalLatency.Milliseconds() / int64(successCount)
		successRate := float64(successCount) / float64(numReqs) * 100
		log.Printf("Statistics: %d/%d successful (%.1f%%), avg latency: %dms",
			successCount, numReqs, successRate, avgLatency)
	} else {
		log.Printf("Statistics: %d/%d successful (0.0%%)", successCount, numReqs)
	}

	if successCount < numReqs {
		os.Exit(1)
	}
}
