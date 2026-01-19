# Distributed Key-Value Store with Raft Consensus
## Learning-Focused Implementation Plan

---

### Project Overview

Build a fault-tolerant, distributed key-value store using the Raft consensus algorithm in Go. This project follows the learning progression from MIT 6.824 Distributed Systems course, providing hands-on experience with distributed systems fundamentals.

**Learning Goals:**
- Understand consensus algorithms and why they're needed
- Master Raft: leader election, log replication, and persistence
- Learn fault tolerance and linearizability concepts
- Practice Go concurrency patterns (goroutines, channels, mutexes)
- Build production-grade distributed systems

---

### Core Concepts to Master

**Before starting, understand:**
1. **CAP Theorem** - Consistency, Availability, Partition tolerance tradeoffs
2. **Linearizability** - Strong consistency guarantee
3. **State Machine Replication** - All replicas execute same operations in same order
4. **Raft Paper** - Sections 2, 3, 4, 5, 7 (https://raft.github.io/raft.pdf)

**Key Raft Components:**
- **Leader Election** - Distributed algorithm to elect unique leader
- **Log Replication** - Leader replicates commands to followers
- **Safety** - At most one leader per term, logs match safety property
- **Persistence** - Crash recovery using persistent storage
- **Log Compaction** - Snapshotting to bound log size

---

## Phase 1: Raft Foundation (Leader Election)

**Duration:** 1-2 weeks | **Focus:** Core Raft state machine and leader election

### Tasks

**1. Project Setup**
```
mkdir -p distribKV/{cmd,pkg/{raft,kvserver,common}}
go mod init github.com/yourusername/distribKV
```

**2. RPC Layer**
- Build simple RPC framework (or use `net/rpc`)
- Implement `RequestVote` RPC with args/reply
- Implement `AppendEntries` RPC (heartbeat only for now)

**3. Raft State Structure**
```go
type Raft struct {
    mu        sync.Mutex
    peers     []*rpc.ClientEnd
    me        int

    // Persistent state (saved to disk)
    currentTerm int
    votedFor    int
    log         []LogEntry

    // Volatile state
    state        State // Follower, Candidate, Leader
    commitIndex  int
    lastApplied  int

    // Leader state
    nextIndex  []int
    matchIndex []int
}
```

**4. Leader Election Logic**
- Random election timeouts (150-300ms)
- Convert to candidate: increment term, vote for self
- Send `RequestVote` to all peers
- Win election: receive votes from majority
- Become follower if discovered higher term

**5. Heartbeats**
- Send periodic `AppendEntries` (empty) as leader
- Reset election timer on receiving RPC

**Tests to Pass:**
- Single leader elected in cluster
- Re-election after leader failure
- Multiple candidates handled correctly
- Followers revert to follower on higher term

**Key Learnings:**
- Distributed election timing issues
- Race conditions in concurrent systems
- Importance of randomized timeouts
- Split vote scenarios

**Testing Commands:**
```bash
go test ./pkg/raft -run TestElection
go test ./pkg/raft -run TestLeaderElection
```

---

## Phase 2: Log Replication

**Duration:** 2-3 weeks | **Focus:** Replicating client commands across cluster

### Tasks

**1. Enhanced AppendEntries**
- Add log entries to RPC
- Implement prevLogIndex/prevLogTerm consistency check
- Leader retries until consistency achieved

**2. Log Management**
- Append entries to leader's log
- Send entries to followers via AppendEntries
- Handle log conflicts (truncate inconsistent entries)
- Track commit index (majority replication)

**3. Client Command Interface**
```go
func (rf *Raft) Start(command interface{}) (index, term int, isLeader bool)
```
- Returns index where command was appended
- Client can poll for commit status
- Apply committed commands to state machine

**4. Apply Channel**
- Send committed entries via channel
- Separate goroutine waits for commitIndex > lastApplied
- Apply entries in order

**Tests to Pass:**
- Commands replicated to all nodes
- Commands committed after majority replication
- Followers apply entries in same order
- Leader failure: new leader has committed entries

**Key Learnings:**
- Log consistency property (Section 5.4 of Raft paper)
- Majority quorum calculations
- Network partition handling
- Leader reverts to follower if not leader

**Testing Commands:**
```bash
go test ./pkg/raft -run TestBasicAgree
go test ./pkg/raft -run TestFailAgree
go test ./pkg/raft -run TestFailNoAgree
```

---

## Phase 3: Persistence

**Duration:** 1 week | **Focus:** Crash recovery using persistent storage

### Tasks

**1. Persister Interface**
```go
type Persister struct {
    mu         sync.Mutex
    raftState  []byte  // Marshallable Raft state
    snapshot   []byte  // Optional
}
```

**2. Persistence Points**
- Before handling RequestVote
- Before handling AppendEntries
- After state changes (becoming leader/candidate)
- After committing/applying entries

**3. Recovery on Startup**
- Read persistent state from disk
- Restore currentTerm, votedFor, log
- Reinitialize volatile state

**4. Snapshot Support**
- Compact log up to lastIncludedIndex
- Store snapshot separately
- First log entry contains snapshot metadata

**Tests to Pass:**
- Raft state persists after crash
- Recovery restores correct term and log
- Multiple crash/recovery cycles handled
- Snapshot compaction works

**Key Learnings:**
- Two-phase commit for persistence
- Atomic write patterns
- Recovery ordering matters
- When to persist vs when not to

**Testing Commands:**
```bash
go test ./pkg/raft -run TestPersist1
go test ./pkg/raft -run TestPersist2
go test ./pkg/raft -run TestPersist3
```

---

## Phase 4: Key-Value Service

**Duration:** 2 weeks | **Focus:** Build KV store on top of Raft

### Tasks

**1. KVServer Structure**
```go
type KVServer struct {
    mu      sync.Mutex
    me      int
    raft    *raft.Raft
    applyCh chan raft.ApplyMsg

    // Data store
    kvStore map[string]string

    // For detecting duplicate requests
    lastSeq  map[int64]int // clientId -> last sequence

    // Dead for test
    killCh chan bool
}
```

**2. Command Types**
```go
type Op struct {
    Operation string // "Put", "Append", "Get"
    Key       string
    Value     string
    ClientId  int64  // For deduplication
    SeqNum    int
}
```

**3. RPC Handlers**
- `Put(key, value)` - Write operation through Raft
- `Append(key, value)` - Append to existing value
- `Get(key)` - Read operation (through Raft for consistency)

**4. Apply Loop**
- Wait for ApplyMsg from Raft
- Apply operation to local kvStore
- Track operation completion (index -> channel)
- Handle duplicate requests (client IDs, sequence numbers)

**5. Clerk (Client)**
```go
type Clerk struct {
    servers []*labrpc.ClientEnd
    leader  int
    clientId int64
    seqNum   int
}
```
- Try each server until finding leader
- Resend request on timeout/leader change
- Handle ErrWrongLeader responses

**Tests to Pass:**
- Basic put/get operations
- Linearizability under failures
- Duplicate request handling
- Concurrent client operations

**Key Learnings:**
- Raft provides linearizability if commands go through consensus
- Client deduplication needed for exactly-once semantics
- Linearizability vs eventual consistency tradeoffs
- How to test distributed systems for linearizability

**Testing Commands:**
```bash
go test ./pkg/kvserver -run TestBasicKV
go test ./pkg/kvserver -run TestConcurrent
go test ./pkg/kvserver -run TestLinearizability
```

---

## Phase 5: Snapshotting

**Duration:** 1-2 weeks | **Focus:** Log compaction and faster recovery

### Tasks

**1. Snapshot RPC**
- `InstallSnapshot` from leader to lagging followers
- Include lastIncludedIndex, lastIncludedTerm, data

**2. KVServer Snapshot**
- Periodically snapshot kvStore state
- Tell Raft to discard log entries before snapshot
- Truncate local log, update Raft state

**3. Snapshot Management**
- Decide when to snapshot (log size threshold)
- Atomic snapshot write
- Handle out-of-date snapshots

**4. Follower Snapshot**
- Receive `InstallSnapshot` if log too old
- Replace state with snapshot
- Discard previous log entries

**Tests to Pass:**
- Snapshot compaction reduces log size
- InstallSnapshot brings lagging followers up to date
- KV state preserved through snapshot
- Multiple snapshots handled correctly

**Key Learnings:**
- Log compaction necessity for long-running systems
- Snapshot format design
- Transfer of large state over network
- Tradeoffs: snapshot size vs frequency

**Testing Commands:**
```bash
go test ./pkg/raft -run TestSnapshotBasic
go test ./pkg/raft -run TestSnapshotInstall
go test ./pkg/kvserver -run TestSnapshotKV
```

---

## Phase 6: Advanced Features (Optional)

**Duration:** 1-2 weeks each | **Focus:** Production-ready features

### A. Configuration Changes
- Dynamic cluster membership (add/remove nodes)
- Joint consensus for safety during reconfiguration
- Use `ConfChange` entries in Raft log

### B. Multi-Raft Sharding
- Partition keys across multiple Raft groups
- Shard migration and rebalancing
- Cross-shard transactions (optional)

### C. Performance Optimization
- Batch log entries
- Pipeline RPCs
- Read-only queries on followers (with safety checks)
- Optimistic concurrency for reads

### D. Monitoring & Metrics
- Leader election latency
- Log replication lag
- Request throughput
- Cluster health monitoring

---

## Testing Strategy

### Unit Tests
Each component has comprehensive unit tests:
- Raft election, replication, persistence
- KVServer operations, apply loop, deduplication
- Clerk retry logic, leader detection

### Integration Tests
- 3-5 node clusters
- Concurrent clients
- Network partitions (kill nodes, restart)
- Crash recovery tests

### Linearizability Testing
Use model checking tool (like Jepsen's porcupine in Go):
```go
// Test that all operations appear to execute atomically at some point
// between their start and end
```

### Stress Tests
- 1000s of operations
- High failure rates
- Long-running durability tests

---

## Directory Structure

```
distribKV/
├── cmd/
│   ├── raft-kv-server/  # Main server binary
│   └── test-client/      # Load testing client
├── pkg/
│   ├── common/
│   │   └── rpc.go       # RPC types and client/server setup
│   ├── raft/
│   │   ├── raft.go      # Main Raft implementation
│   │   ├── election.go  # Leader election logic
│   │   ├── replication.go  # Log replication
│   │   ├── persistence.go  # State persistence
│   │   └── raft_test.go # Comprehensive tests
│   ├── kvserver/
│   │   ├── server.go    # KV service on Raft
│   │   ├── clerk.go     # Client library
│   │   └── server_test.go
│   └── snapshot/
│       └── snapshot.go  # Snapshot management
├── configs/
│   └── cluster.yaml     # Cluster configuration
└── scripts/
    ├── start-cluster.sh # Start 3-node cluster
    └── test-cluster.sh # Run tests
```

---

## Build & Test Commands

```bash
# Initialize Go module
go mod init github.com/yourusername/distribKV
go mod tidy

# Build server
go build -o bin/kvserver ./cmd/raft-kv-server

# Run single test
go test ./pkg/raft -run TestLeaderElection -v

# Run all Raft tests (with race detector)
go test -race ./pkg/raft

# Run all KVServer tests
go test -race ./pkg/kvserver

# Run full test suite
go test ./...

# Start 3-node cluster (manually)
./bin/kvserver -id 1 -peers localhost:10001,localhost:10002,localhost:10003 &
./bin/kvserver -id 2 -peers localhost:10001,localhost:10002,localhost:10003 &
./bin/kvserver -id 3 -peers localhost:10001,localhost:10002,localhost:10003 &

# Test with curl
curl -X PUT localhost:10001/put -d '{"key":"foo","value":"bar"}'
curl localhost:10001/get/foo
```

---

## Key Resources

### Required Reading
1. **Raft Paper** - https://raft.github.io/raft.pdf (read multiple times!)
2. **MIT 6.824 Raft Lab** - https://pdos.csail.mit.edu/6.824/labs/lab-raft.html
3. **Raft Visualization** - https://raft.github.io/

### Reference Implementations
- MIT 6.824 starter code (labrpc, persister)
- Etcd's Raft library (go.etcd.io/etcd/raft) - production-grade
- HashiCorp Raft (github.com/hashicorp/raft)

### Videos
- MIT 6.824 Lecture 5: Go, Threads, and Raft
- MIT 6.824 Lecture 6-7: Fault Tolerance (Raft deep dive)

### Testing Tools
- Jepsen's porcupine (https://github.com/anuraaga/porcupine)
- Maelstrom (for asynchronous network testing)

---

## Common Pitfalls & Debugging Tips

1. **Don't underestimate concurrent bugs**
   - Always lock before accessing shared state
   - Use `defer mutex.Unlock()` for safety
   - Run tests with `-race` flag

2. **Election timeout issues**
   - Make timeouts truly random (use math/rand with seed)
   - Range: 150-300ms (not 150ms exactly)
   - Reset timer on receiving RPC

3. **Log replication bugs**
   - Check prevLogIndex/prevLogTerm carefully
   - Must handle index out of bounds
   - Followers must respond to heartbeats

4. **Committing too early**
   - Only commit when majority has log entry
   - Can't commit from previous term if not majority
   - Section 5.4.2 in Raft paper

5. **Persistence timing**
   - Persist before responding to RPCs
   - Restore state before accepting RPCs
   - Check persisted state on startup

6. **Deadlocks**
   - Never call RPCs while holding locks
   - Be careful with channel operations in goroutines
   - Add timeouts to blocking operations

---

## Success Metrics

By the end of this project, you should:

✅ **Understand** Raft's leader election, log replication, and persistence
✅ **Implement** a fully functional distributed KV store
✅ **Test** with linearizability verification (Jepsen-style)
✅ **Handle** network partitions, crashes, and concurrent clients
✅ **Explain** tradeoffs between consistency, availability, and performance
✅ **Appreciate** challenges in distributed systems development

---

## Timeline Estimate

- Phase 1 (Raft Foundation): 1-2 weeks
- Phase 2 (Log Replication): 2-3 weeks
- Phase 3 (Persistence): 1 week
- Phase 4 (KV Service): 2 weeks
- Phase 5 (Snapshotting): 1-2 weeks
- Phase 6 (Advanced features): Optional 2-4 weeks

**Total: 7-12 weeks** for complete implementation

---

## Next Steps

1. Read Raft paper (Sections 1-5, 7)
2. Set up Go environment and project structure
3. Start Phase 1: Implement basic Raft state machine
4. Work through tests incrementally
5. Don't skip the testing - it's crucial for correctness!
6. Ask for help early when stuck - distributed systems are hard

---

**Good luck! Building a Raft-based system is challenging but incredibly rewarding.**
