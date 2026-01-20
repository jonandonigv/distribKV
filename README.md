# distribKV

A distributed key-value store built with the Raft consensus algorithm in Go. This is a learning-focused implementation following the MIT 6.824 Distributed Systems course.

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
go build -o bin/kvserver ./cmd/raft-kv-server
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

## Project Structure

```
distribKV/
├── cmd/
│   └── raft-kv-server/    # Main server binary
├── pkg/
│   ├── common/
│   │   └── grpc.go        # gRPC utilities
│   ├── raft/
│   │   ├── raft.go        # Main Raft implementation
│   │   ├── election.go    # Leader election logic
│   │   ├── replication.go  # Log replication
│   │   └── persistence.go # State persistence
│   └── kvserver/
│       ├── server.go      # KV service on Raft
│       └── clerk.go       # Client library
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

This is a learning project. Follow the guidelines in [AGENTS.md](./AGENTS.md) for code style and testing.

## License

MIT
