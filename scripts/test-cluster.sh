#!/bin/bash

# Test the KV cluster
# Usage: ./scripts/test-cluster.sh

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

PEERS="localhost:10001,localhost:10002,localhost:10003"

echo -e "${BLUE}Testing KV Cluster...${NC}"
echo ""

if [ ! -f "./bin/kv-client" ]; then
    echo -e "${RED}Error: ./bin/kv-client not found${NC}"
    echo -e "${YELLOW}Please build first: make build or go build -o bin/kv-client ./cmd/kv-client${NC}"
    exit 1
fi

./bin/kv-client -peers="${PEERS}" test

echo ""
echo -e "${GREEN}✓ All cluster tests passed!${NC}"
