#!/bin/bash

# Start a 3-node KV cluster
# Usage: ./scripts/start-cluster.sh [-v|--verbose]

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Parse arguments
VERBOSE=""
if [[ "$1" == "-v" || "$1" == "--verbose" ]]; then
    VERBOSE="-verbose"
    echo -e "${YELLOW}Starting cluster in verbose mode${NC}"
fi

# Server configuration
PEERS="localhost:10001,localhost:10002,localhost:10003"
DATA_DIR="./data"

# Create data directories
mkdir -p ${DATA_DIR}/server1
mkdir -p ${DATA_DIR}/server2
mkdir -p ${DATA_DIR}/server3

echo -e "${BLUE}Starting 3-node KV cluster...${NC}"
echo -e "${BLUE}Peers: ${PEERS}${NC}"
echo ""

# Function to cleanup processes on exit
cleanup() {
    echo ""
    echo -e "${YELLOW}Shutting down cluster...${NC}"
    
    if [ -n "$PID1" ]; then
        kill $PID1 2>/dev/null || true
        wait $PID1 2>/dev/null || true
        echo -e "${GREEN}✓ Server 1 stopped${NC}"
    fi
    
    if [ -n "$PID2" ]; then
        kill $PID2 2>/dev/null || true
        wait $PID2 2>/dev/null || true
        echo -e "${GREEN}✓ Server 2 stopped${NC}"
    fi
    
    if [ -n "$PID3" ]; then
        kill $PID3 2>/dev/null || true
        wait $PID3 2>/dev/null || true
        echo -e "${GREEN}✓ Server 3 stopped${NC}"
    fi
    
    echo -e "${GREEN}Cluster shutdown complete${NC}"
    exit 0
}

# Set trap to cleanup on Ctrl+C
trap cleanup SIGINT SIGTERM

# Check if binary exists
if [ ! -f "./bin/kvserver" ]; then
    echo -e "${RED}Error: ./bin/kvserver not found${NC}"
    echo -e "${YELLOW}Please build first: go build -o bin/kvserver ./cmd/kvserver${NC}"
    exit 1
fi

# Kill any existing servers
pkill -f "kvserver" 2>/dev/null || true
sleep 0.5

echo -e "${BLUE}Starting all 3 servers simultaneously...${NC}"

# Start ALL servers at once (in parallel) to avoid connection race
./bin/kvserver -id=1 -peers="${PEERS}" -data="${DATA_DIR}/server1" ${VERBOSE} > ${DATA_DIR}/server1.log 2>&1 &
PID1=$!

./bin/kvserver -id=2 -peers="${PEERS}" -data="${DATA_DIR}/server2" ${VERBOSE} > ${DATA_DIR}/server2.log 2>&1 &
PID2=$!

./bin/kvserver -id=3 -peers="${PEERS}" -data="${DATA_DIR}/server3" ${VERBOSE} > ${DATA_DIR}/server3.log 2>&1 &
PID3=$!

# Wait a bit for servers to initialize
sleep 2

# Check if all servers are still running
if ! kill -0 $PID1 2>/dev/null; then
    echo -e "${RED}✗ Server 1 failed to start${NC}"
    echo -e "${YELLOW}Check logs: ${DATA_DIR}/server1.log${NC}"
    cleanup
fi

if ! kill -0 $PID2 2>/dev/null; then
    echo -e "${RED}✗ Server 2 failed to start${NC}"
    echo -e "${YELLOW}Check logs: ${DATA_DIR}/server2.log${NC}"
    cleanup
fi

if ! kill -0 $PID3 2>/dev/null; then
    echo -e "${RED}✗ Server 3 failed to start${NC}"
    echo -e "${YELLOW}Check logs: ${DATA_DIR}/server3.log${NC}"
    cleanup
fi

echo ""
echo -e "${GREEN}✓ All servers started${NC}"
echo ""
echo -e "${YELLOW}Cluster Status:${NC}"
echo -e "  Server 1: localhost:10001 (PID: $PID1)"
echo -e "  Server 2: localhost:10002 (PID: $PID2)"
echo -e "  Server 3: localhost:10003 (PID: $PID3)"
echo ""
echo -e "${YELLOW}Log files:${NC}"
echo -e "  ${DATA_DIR}/server1.log"
echo -e "  ${DATA_DIR}/server2.log"
echo -e "  ${DATA_DIR}/server3.log"
echo ""
echo -e "${BLUE}Press Ctrl+C to stop the cluster${NC}"
echo ""

# Show logs in real-time (optional)
if [ -n "$VERBOSE" ]; then
    echo -e "${YELLOW}Showing logs (Ctrl+C to stop)...${NC}"
    tail -f ${DATA_DIR}/server1.log ${DATA_DIR}/server2.log ${DATA_DIR}/server3.log &
    TAIL_PID=$!
    
    # Wait for interrupt
    wait $PID1 $PID2 $PID3 2>/dev/null || true
    kill $TAIL_PID 2>/dev/null || true
else
    # Wait for all background processes
    wait $PID1 $PID2 $PID3 2>/dev/null || true
fi

# Cleanup
cleanup
