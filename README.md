# distribKV

A distributed key-value store built with the Raft consensus algorithm in Go. While initially developed as a learning project following the MIT 6.824 Distributed Systems course, this implementation is architected with production use in mind and will be hardened for production deployment.

## Overview

distribKV provides a fault-tolerant, strongly consistent key-value store through Raft consensus. All nodes agree on the order of operations, ensuring linearizability even during network partitions and node failures.

**Key Features:**

- ✅ Raft consensus for leader election and log replication
- ✅ Strong consistency (linearizable reads/writes)
- ✅ Fault tolerance with automatic leader election
- ✅ Crash recovery with state persistence
- ✅ Key-Value service layer (Put/Get/Append)
- ✅ Client library with automatic retry and leader discovery
- ✅ gRPC-based communication with keepalive
- ✅ Stable cluster operation (tested 2+ minutes continuous operation)
- 🔄 Log compaction via snapshotting (planned)

## Current Implementation Status

### ✅ Stable - Production Ready

**Core Raft**

- Randomized timeouts (150-300ms)
- Vote counting with proper mutex protection
- Timer reset on valid leader communication
- Election safety: only one leader per term
- **Election backoff for failed elections** (prevents thrashing)
- **Proper election tracking with pending RPC counting**
- Heartbeat sender (50ms intervals)
- AppendEntries RPC with log matching
- Conflict detection and log truncation
- nextIndex/matchIndex tracking per peer
- Automatic retry on mismatch

**gRPC & Networking**

- **Keepalive configuration (10s ping, 3s timeout)**
- **Connection state checking before RPCs**
- **Reduced RPC timeouts (500ms RequestVote, 1s AppendEntries)**
- Health check service

**Persistence Layer**

- JSON format with base64 encoding
- Atomic writes (temp file + fsync + rename)
- Automatic state recovery on startup
- Data directory: `./data/raft-state.json`

**State Machine Integration**

- Apply channel for committed entries
- Background apply goroutine
- Proper lastApplied tracking

**Key-Value Service**

- Get, Put, Append operations
- Duplicate detection and caching (100 entries or 10s per client)
- Leader tracking and hints
- Thread-safe concurrent operations

**Client Library (Clerk)**

- Automatic leader discovery
- Exponential backoff retry (50ms → 1s)
- Sequence numbers for exactly-once semantics
- Persistent connections to all servers
- 1000 attempt limit with panic on total failure

### 🔄 In Progress / Planned

**Future Work**

- Log compaction and snapshotting
- Comprehensive test suite
- Production deployment hardening

## Architecture

### Two-Layer Design

```
┌─────────────────────────────────────────┐
│         APPLICATION LAYER               │
│                                         │
│   ck.Get("foo")                         │
│   ck.Put("bar", "baz")                  │
│   ck.Append("key", "-suffix")           │
│                                         │
└──────────────┬──────────────────────────┘
               │
┌──────────────▼──────────────────────────┐
│              CLERK                      │
│                                         │
│  • Leader caching & discovery           │
│  • Automatic retry with backoff         │
│  • Sequence number tracking             │
│  • Connection management                │
│                                         │
└──────────────┬──────────────────────────┘
               │ gRPC
┌──────────────▼──────────────────────────┐
│            KV CLUSTER                   │
│                                         │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐ │
│  │Server 1 │  │Server 2 │  │Server 3 │ │
│  │ (Raft)  │  │ (Raft)  │  │ (Raft)  │ │
│  └─────────┘  └─────────┘  └─────────┘ │
│                                         │
└─────────────────────────────────────────┘
```

### Consensus & Raft

Raft is a consensus algorithm that allows multiple servers to agree on values. Once they reach a decision, that decision is final. Raft provides:

- **Leader Election** - Distributed algorithm to elect a unique leader
- **Log Replication** - Leader replicates commands to followers
- **Safety** - At most one leader per term, logs match safety property
- **Persistence** - Crash recovery using persistent storage

### CAP Theorem

In any distributed data store, you can only provide two of three guarantees:

- **Consistency** - All nodes see the same data at the same time
- **Availability** - Every request receives a response
- **Partition Tolerance** - System continues despite network failures

When a network partition occurs, you must choose between consistency or availability.

### Linearizability

Linearizability provides the illusion that:

- There's only one copy of the data
- Operations execute one at a time
- The system has a global, instantaneous order of operations

## Getting Started

### Prerequisites

- Go 1.25.5 or later
- Protocol Buffers compiler (`protoc`) for gRPC code generation

### Installation

```bash
# Clone the repository
git clone https://github.com/jonandonigv/distribKV.git
cd distribKV

# Install dependencies
go mod tidy
```

### Build

```bash
# Build the KV server
go build -o bin/kvserver ./cmd/kvserver

# Build all binaries
go build -o bin/ ./cmd/...
```

### Running a Cluster

#### Quick Start with Scripts

The easiest way to run a cluster is using the provided scripts:

```bash
# Start a 3-node cluster
./scripts/start-cluster.sh

# Or with verbose logging
./scripts/start-cluster.sh -v

# Test the cluster
./scripts/test-cluster.sh
```

The startup script will:

- Start all 3 servers simultaneously
- Create separate data directories for each node
- Show server status and log locations
- Handle graceful shutdown on Ctrl+C

#### Manual Startup

If you prefer to start servers individually:

```bash
# Terminal 1 - Node 1
./bin/kvserver -id=1 -peers="localhost:10001,localhost:10002,localhost:10003" -verbose

# Terminal 2 - Node 2
./bin/kvserver -id=2 -peers="localhost:10001,localhost:10002,localhost:10003" -verbose

# Terminal 3 - Node 3
./bin/kvserver -id=3 -peers="localhost:10001,localhost:10002,localhost:10003" -verbose
```

**Note:** When starting manually, you must start all 3 servers simultaneously (within ~1 second) because the Raft layer requires all peers to be reachable at startup.

**Server Options:**

- `-id` - Server ID (optional, derived from port if not specified)
- `-peers` - Comma-separated list of peer addresses (required)
- `-port` - Port to listen on (default: 10000 + id)
- `-verbose` - Enable verbose logging
- `-data` - Data directory for persistence (default: ./data)

### Using the Client Library

```go
package main

import (
    "fmt"
    "github.com/jonandonigv/distribKV/pkg/kvserver"
)

func main() {
    // Create clerk (client)
    ck := kvserver.MakeClerk([]string{
        "localhost:10001",
        "localhost:10002",
        "localhost:10003",
    }, false) // verbose=false

    // Store a value
    ck.Put("greeting", "Hello")

    // Append to it
    ck.Append("greeting", " World")

    // Retrieve the value
    value := ck.Get("greeting")
    fmt.Println(value) // Output: Hello World
}
```

### Running Tests

```bash
# Run all tests with race detector
go test -race ./...

# Run a specific test
go test ./pkg/raft -run TestElection -v

# Run tests with coverage
go test -race -cover ./...
```

## Project Structure

```
distribKV/
├── cmd/
│   ├── kvserver/             # Main KV server binary
│   │   └── main.go
│   ├── kv-client/            # KV cluster test client
│   │   └── main.go
│   ├── grpc-test-server/     # gRPC Health Check server (testing)
│   │   └── main.go
│   └── grpc-test-client/     # gRPC Health Check client (testing)
│       └── main.go
├── pkg/
│   ├── common/
│   │   └── grpc.go           # gRPC server/client utilities
│   ├── health/
│   │   └── health.go         # Health Check service implementation
│   ├── raft/                 # Core Raft implementation
│   │   ├── raft.go           # Main Raft struct and initialization
│   │   ├── election.go       # Leader election and timers
│   │   ├── replication.go    # Log replication and heartbeat
│   │   └── persistance.go    # State persistence to disk
│   ├── kvserver/             # KV service implementation
│   │   ├── server.go         # KVServer with RPC handlers
│   │   ├── clerk.go          # Client library
│   │   ├── apply.go          # Apply loop for state machine
│   │   └── types.go          # Type definitions
│   └── snapshot/             # Snapshotting utilities
│       └── snapshot.go
├── proto/
│   ├── kv.proto              # KV service definitions
│   ├── kv.pb.go              # Generated KV messages
│   ├── kv_grpc.pb.go         # Generated KV service interfaces
│   ├── raft.proto            # Raft RPC definitions
│   ├── raft/
│   │   ├── raft.pb.go        # Generated Raft messages
│   │   └── raft_grpc.pb.go   # Generated Raft service interfaces
│   └── health.proto          # Health Check service definition
├── bin/
│   ├── kvserver              # Compiled KV server
│   ├── grpc-test-server      # Compiled test server
│   └── grpc-test-client      # Compiled test client
├── data/                     # Persistent state storage
│   ├── server1.log           # Server 1 logs
│   ├── server2.log           # Server 2 logs
│   └── server3.log           # Server 3 logs
├── scripts/                  # Utility scripts
│   ├── start-cluster.sh      # Start 3-node cluster
│   └── test-cluster.sh       # Test cluster operations
├── Makefile                  # Build convenience targets
├── go.mod                    # Go module definition
└── README.md                 # This file
```

## API Reference

### Client Library (Clerk)

The Clerk provides a simple API for interacting with the KV cluster:

#### Creating a Clerk

```go
import "github.com/jonandonigv/distribKV/pkg/kvserver"

// Create clerk connected to all servers
ck := kvserver.MakeClerk([]string{
    "localhost:10001",
    "localhost:10002",
    "localhost:10003",
}, false) // verbose=false for production
```

#### Put Operation

```go
// Store a key-value pair
// Panics if operation fails after 1000 attempts
ck.Put("mykey", "myvalue")
```

#### Get Operation

```go
// Retrieve a value
// Returns value or panics on failure
value := ck.Get("mykey")
```

#### Append Operation

```go
// Append value to existing key
// Equivalent to: value = value + suffix
ck.Append("mykey", "-suffix")
```

### Core Raft API

For advanced use cases, you can interact with the Raft layer directly:

#### Creating a Raft Node

```go
import (
    "context"
    "time"
    "github.com/jonandonigv/distribKV/pkg/raft"
)

// Create a new Raft node
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

r, err := raft.NewRaft(
    1,  // Server ID
    []string{"localhost:10001", "localhost:10002", "localhost:10003"},  // Peer addresses
    ctx,  // Connection context
)
if err != nil {
    log.Fatal(err)
}

// Start the election timer
r.Start()
```

#### Submitting Commands

```go
// Submit a command to the Raft log
command := []byte("your-command-data")
index, err := r.ReplicateCommand(command)

if err == raft.ErrNotLeader {
    // This node is not the leader
    log.Println("Not leader, retry with another node")
    return
}

if err == raft.ErrTimeout {
    // Timeout waiting for commit (5 seconds)
    log.Printf("Timeout, but command may still commit at index %d", index)
    return
}

if err != nil {
    log.Printf("Error: %v", err)
    return
}

log.Printf("Command committed at index %d", index)
```

#### Receiving Applied Commands

```go
// Get the apply channel
applyCh := r.GetApplyCh()

// Read committed commands
for msg := range applyCh {
    if msg.CommandValid {
        fmt.Printf("Applying command at index %d: %v\n",
            msg.CommandIndex, msg.Command)
    }
}
```

## Implementation Details

### Duplicate Detection

The KV server tracks completed operations using (clientId, sequenceNum) pairs:

- Cache size: 100 entries per client
- Expiration: 10 seconds
- Eviction: FIFO when limit reached

This ensures exactly-once semantics even with client retries.

### Leader Discovery

The Clerk maintains a leader cache:

- Tries cached leader first for optimization
- Updates cache on wrong_leader responses
- Falls back to round-robin if leader unknown
- Clears cache on election timeout

### Retry Strategy

All operations use exponential backoff:

- Attempt 1: 50ms
- Attempt 2: 100ms
- Attempt 3: 200ms
- Attempt 4: 400ms
- Attempt 5: 800ms
- Attempt 6+: 1s (capped)

Maximum 1000 attempts before panic (indicates total cluster failure).

### Thread Safety

The Clerk is safe for concurrent use:

- Sequence numbers use mutex protection
- Leader cache updates are atomic
- Multiple goroutines can share one Clerk instance

## License

MIT
