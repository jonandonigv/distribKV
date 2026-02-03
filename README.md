# distribKV

A distributed key-value store built with the Raft consensus algorithm in Go. While initially developed as a learning project following the MIT 6.824 Distributed Systems course, this implementation is architected with production use in mind and will be hardened for production deployment.

## Overview

distribKV provides a fault-tolerant, strongly consistent key-value store through Raft consensus. All nodes agree on the order of operations, ensuring linearizability even during network partitions and node failures.

**Key Features:**
- Raft consensus for leader election and log replication
- Strong consistency (linearizable reads/writes)
- Fault tolerance with automatic leader election
- Crash recovery with state persistence
- Log compaction via snapshotting

## Background

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
# Using make (recommended)
make build

# Or build individual components
make server  # Build grpc-test-server
make client  # Build grpc-test-client

# Build all binaries
go build -o bin/ ./cmd/...
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

### Starting a Cluster

```bash
# Start 3-node cluster (in separate terminals)
./bin/kvserver -id 1 -peers localhost:10001,localhost:10002,localhost:10003 &
./bin/kvserver -id 2 -peers localhost:10001,localhost:10002,localhost:10003 &
./bin/kvserver -id 3 -peers localhost:10001,localhost:10002,localhost:10003 &
```

## Testing gRPC Infrastructure

The project includes CLI tools to test the gRPC communication layer using a standard Health Check service.

### Start Server

```bash
# Start on default port (50051)
./bin/grpc-test-server

# Start on custom port with custom ID
./bin/grpc-test-server --port=10001 --id=server-1
```

Server options:
- `--port` - Server port (default: 50051)
- `--id` - Server identifier for logging (default: "1")

### Run Client

**Single Request Mode (default):**

```bash
./bin/grpc-test-client --addr=localhost:50051 --id=client-1
```

Single mode performs one health check and exits with status 0 on success, 1 on failure.

**Continuous Request Mode:**

```bash
# Send 10 requests with 500ms delay between requests
./bin/grpc-test-client --addr=localhost:50051 --mode=continuous --requests=10 --delay=500ms --id=load-test-1
```

Continuous mode options:
- `--mode` - "single" or "continuous" (default: single)
- `--requests` - Number of requests to send (continuous mode, default: 1)
- `--delay` - Delay between requests (continuous mode, default: 1s)
- `--addr` - Server address (default: localhost:50051)
- `--id` - Client identifier for logging (default: "client")

### Multi-Client Testing

Test concurrent client connections:

```bash
# Terminal 1: Start server
./bin/grpc-test-server --port=50051 --id=server

# Terminal 2: Start client 1
./bin/grpc-test-client --id=client-1 --mode=continuous --requests=100 --delay=200ms &

# Terminal 3: Start client 2
./bin/grpc-test-client --id=client-2 --mode=continuous --requests=100 --delay=300ms &

# Terminal 4: Start client 3
./bin/grpc-test-client --id=client-3 --mode=continuous --requests=100 --delay=400ms &
```

Each client logs its own statistics including success rate and average latency.

### Graceful Shutdown

Both server and client respond to `SIGINT` (Ctrl+C) and `SIGTERM` signals gracefully.

```bash
# Server logs shutdown sequence
[test-server] 2026/01/22 12:32:49 Health check server listening on port 50051
^C[test-server] 2026/01/22 12:32:52 Shutting down server...
[test-server] 2026/01/22 12:32:52 Server stopped gracefully
```

Client in continuous mode reports statistics before shutting down.

## Project Structure

```
distribKV/
├── cmd/
│   ├── grpc-test-server/   # gRPC Health Check server (testing)
│   │   └── main.go
│   ├── grpc-test-client/   # gRPC Health Check client (testing)
│   │   └── main.go
│   └── raft-kv-server/    # Main KV server (future)
├── pkg/
│   ├── common/
│   │   └── grpc.go        # gRPC server/client utilities
│   ├── health/
│   │   └── health.go      # Health Check service implementation
│   ├── raft/
│   │   ├── raft.go        # Main Raft implementation
│   │   ├── election.go    # Leader election logic
│   │   ├── replication.go  # Log replication
│   │   └── persistence.go # State persistence
│   └── kvserver/
│       ├── server.go      # KV service on Raft
│       └── clerk.go       # Client library
├── proto/
│   ├── health.proto       # Health Check service definition
│   ├── health.pb.go      # Generated messages
│   └── health_grpc.pb.go # Generated service interfaces
├── bin/
│   ├── grpc-test-server   # Compiled server binary
│   └── grpc-test-client   # Compiled client binary
├── Makefile               # Build convenience targets
├── configs/
│   └── cluster.yaml       # Cluster configuration
└── scripts/
    ├── start-cluster.sh   # Start multi-node cluster
    └── test-cluster.sh    # Run tests
```

## Usage

### Basic Operations

```go
// Create client
clerk := kvserver.NewClerk([]string{"localhost:10001", "localhost:10002", "localhost:10003"})

// Put operation
clerk.Put("foo", "bar")

// Get operation
value := clerk.Get("foo") // returns "bar"

// Append operation
clerk.Append("foo", "-baz") // foo becomes "bar-baz"
```

## Learning Phases

1. **Phase 1**: Raft Foundation - Leader election and heartbeats
2. **Phase 2**: Log Replication - Replicate client commands across cluster
3. **Phase 3**: Persistence - Crash recovery using persistent storage
4. **Phase 4**: Key-Value Service - Build KV store on top of Raft
5. **Phase 5**: Snapshotting - Log compaction and faster recovery

## Resources

### Required Reading
- [Raft Paper](https://raft.github.io/raft.pdf) - Sections 2, 3, 4, 5, 7
- [MIT 6.824 Raft Lab](https://pdos.csail.mit.edu/6.824/labs/lab-raft.html)
- [Raft Visualization](https://raft.github.io/)

### Reference Implementations
- [Etcd's Raft library](https://github.com/etcd-io/etcd/tree/main/server/etcdserver/api/raft)
- [HashiCorp Raft](https://github.com/hashicorp/raft)

## Contributing

This project is being developed with production deployment as a goal. Follow the guidelines in [AGENTS.md](./AGENTS.md) for code style and testing.

When making changes to gRPC infrastructure:
1. Test with `grpc-test-server` and `grpc-test-client` CLI tools
2. Verify multi-client scenarios work correctly
3. Ensure graceful shutdown on signals
4. Run tests with race detector: `go test -race ./...`

## License

MIT
