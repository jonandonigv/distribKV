#!/bin/bash

# Test the KV cluster
# Usage: ./scripts/test-cluster.sh

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}Testing KV Cluster...${NC}"
echo ""

# Create a simple test program
cat > /tmp/kv-test.go << 'EOF'
package main

import (
	"fmt"
	"log"
	"github.com/jonandonigv/distribKV/pkg/kvserver"
)

func main() {
	ck := kvserver.MakeClerk([]string{
		"localhost:10001",
		"localhost:10002",
		"localhost:10003",
	}, false)

	fmt.Println("Testing Put operation...")
	ck.Put("test-key", "test-value")
	fmt.Println("✓ Put successful")

	fmt.Println("Testing Get operation...")
	value := ck.Get("test-key")
	if value != "test-value" {
		log.Fatalf("Expected 'test-value', got '%s'", value)
	}
	fmt.Println("✓ Get successful, value:", value)

	fmt.Println("Testing Append operation...")
	ck.Append("test-key", "-appended")
	value = ck.Get("test-key")
	if value != "test-value-appended" {
		log.Fatalf("Expected 'test-value-appended', got '%s'", value)
	}
	fmt.Println("✓ Append successful, value:", value)

	fmt.Println("")
	fmt.Println("All tests passed!")
}
EOF

echo "Building test program..."
go run /tmp/kv-test.go

rm /tmp/kv-test.go

echo ""
echo -e "${GREEN}✓ All cluster tests passed!${NC}"
