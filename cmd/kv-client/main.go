package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jonandonigv/distribKV/pkg/kvserver"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	peers := parsePeersFlag()
	if len(peers) == 0 {
		log.Fatal("Must specify -peers")
	}

	command := parseCommand()
	if command == "" {
		printUsage()
		os.Exit(1)
	}

	verbose := hasFlag("-v") || hasFlag("--verbose")

	ck := kvserver.MakeClerk(peers, verbose)

	switch command {
	case "test":
		runTest(ck)
	case "get":
		runGet(ck)
	case "put":
		runPut(ck)
	case "append":
		runAppend(ck)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: kv-client -peers=<addrs> <command> [args]

Commands:
  test              Run full test suite (Put, Get, Append)
  get -key=<key>    Get value for key
  put -key=<k> -value=<v>   Put key-value pair
  append -key=<k> -value=<v>  Append value to key

Flags:
  -peers=<addrs>    Comma-separated peer addresses (required)
  -v, -verbose      Enable verbose logging

Examples:
  kv-client -peers=localhost:10001,localhost:10002,localhost:10003 test
  kv-client -peers=localhost:10001,localhost:10002,localhost:10003 get -key=foo
  kv-client -peers=localhost:10001,localhost:10002,localhost:10003 put -key=foo -value=bar
`)
}

func parsePeersFlag() []string {
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-peers=") {
			peers := strings.TrimPrefix(arg, "-peers=")
			return strings.Split(peers, ",")
		}
	}
	return nil
}

func parseCommand() string {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return ""
}

func hasFlag(flag string) bool {
	for _, arg := range os.Args {
		if arg == flag {
			return true
		}
	}
	return false
}

func parseFlag(name string) string {
	prefix := "-" + name + "="
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func runTest(ck *kvserver.Clerk) {
	fmt.Println("Testing KV Cluster...")
	fmt.Println()

	fmt.Println("Testing Put operation...")
	ck.Put("test-key", "test-value")
	fmt.Println("✓ Put successful")

	fmt.Println("Testing Get operation...")
	value := ck.Get("test-key")
	if value != "test-value" {
		log.Fatalf("Expected 'test-value', got '%s'", value)
	}
	fmt.Printf("✓ Get successful, value: %s\n", value)

	fmt.Println("Testing Append operation...")
	ck.Append("test-key", "-appended")
	value = ck.Get("test-key")
	if value != "test-value-appended" {
		log.Fatalf("Expected 'test-value-appended', got '%s'", value)
	}
	fmt.Printf("✓ Append successful, value: %s\n", value)

	fmt.Println()
	fmt.Println("All tests passed!")
}

func runGet(ck *kvserver.Clerk) {
	key := parseFlag("key")
	if key == "" {
		log.Fatal("Must specify -key")
	}

	start := time.Now()
	value := ck.Get(key)
	latency := time.Since(start)

	fmt.Printf("%s (latency: %dms)\n", value, latency.Milliseconds())
}

func runPut(ck *kvserver.Clerk) {
	key := parseFlag("key")
	value := parseFlag("value")
	if key == "" || value == "" {
		log.Fatal("Must specify -key and -value")
	}

	start := time.Now()
	ck.Put(key, value)
	latency := time.Since(start)

	fmt.Printf("✓ Put %s=%s (latency: %dms)\n", key, value, latency.Milliseconds())
}

func runAppend(ck *kvserver.Clerk) {
	key := parseFlag("key")
	value := parseFlag("value")
	if key == "" || value == "" {
		log.Fatal("Must specify -key and -value")
	}

	start := time.Now()
	ck.Append(key, value)
	latency := time.Since(start)

	fmt.Printf("✓ Append to %s (latency: %dms)\n", key, latency.Milliseconds())
}
