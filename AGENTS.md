# AGENTS.md

This file contains instructions for agentic coding assistants working on this repository.

## Project Overview

Distributed Key-Value Store on top of the Raft consensus algorithm in Go. Learning-focused portfolio project that follows TDD.

Active rebuild is on `0.1.0`; the previous `0.0.x` implementation is archived in `archive/test-harness-v1` for reference.

## Project Structure

```
distribKV/
├── cmd/
│   └── kvserver/      # single binary (server + healthcheck subcommand)
├── config/            # cluster.yaml parsing
├── raft/              # consensus engine; *_test.go files include the test harness
├── kv/                # KV state machine + Clerk client library
├── server/            # binary wiring (run.go, grpc.go)
├── health/            # Health gRPC service impl
├── proto/              # .proto source files (raft, kv, health)
├── configs/
│   └── cluster.yaml   # canonical 3-node cluster definition
├── docker-compose.yml # spins 3 kvserver replicas with healthchecks
├── Dockerfile         # multi-stage Go builder; runtime = distroless/static
├── Makefile           # proto/build/test/run/cluster targets
└── ...reference docs (project.md, Architecture.png, CONTRIBUTING.md, Notes/)
```

Generated `.pb.go` files live in the consuming package:
- `kv/kv.pb.go` and `kv/kv_grpc.pb.go`
- `health/health.pb.go` and `health/health_grpc.pb.go`
- `raft/raftpb/raft.pb.go` and `raft/raftpb/raft_grpc.pb.go` (subdirectory, not the raft package itself — see below)

**Why raft uses a `raftpb/` subpackage**: the proto-generated `LogEntry` embeds `protoimpl.MessageState` (which contains a `sync.Mutex`). Ranging over a `[]LogEntry` slice value-copies the entry and trips `go vet` ("range var copies lock"). Since the Raft package ranges over its log slice constantly, we keep our own domain `raft.LogEntry` (plain struct, no mutex) for in-package use and convert to/from `raftpb.LogEntry` only at the RPC boundary in `election.go` and `replication.go`. KV and Health don't have this issue (no per-message slice iteration) so their generated files stay flat in the consumer package. Reference: etcd's `raft/raftpb` and HashiCorp Raft follow the same split.

## Build, Lint, and Test Commands

```bash
make proto              # regenerate all .pb.go from .proto (never hand-edit .pb.go)
make build              # build ./cmd/kvserver to bin/
make fmt                # gofmt -w .
make tidy               # go mod tidy

# Testing (mandatory -race)
go test -race ./...
go test -race -cover ./...
go test -race ./raft -run TestElection -v
make test               # = go test -race -cover ./...
make test-cover         # open HTML coverage report

# Cluster
make run                # 3 background processes locally
make cluster-up         # docker-compose up -d
make cluster-down       # docker-compose down
make cluster-logs       # docker-compose logs -f
make smoke              # in-process Clerk against a running cluster

# Coverage is reported but not enforced in CI (CONTRIBUTING.md's >80% is a target).
```

## Code Style Guidelines

### Imports

- Group imports: stdlib, third-party, project (blank line between groups)
- Sort alphabetically within each group

```go
import (
	"context"
	"sync"

	"google.golang.org/grpc"

	"github.com/jonandonigv/distribKV/raft"
)
```

### Formatting

- Use `gofmt -w .` for formatting
- Use tabs for indentation
- Max line length: 100 characters
- No trailing whitespace

### Types

- Use named types for important concepts (`type State int`, `type LogEntry struct`)
- Prefer pointers for large structs
- Minimize `interface{}`; use specific types or generics
- Use interfaces (`Persister`) to model behaviour, not data. Add them when you have multiple implementations or need to decouple a dependency.

### Naming Conventions

- Package names: lowercase, single word (`config`, `raft`, `kv`, `health`)
- Public: PascalCase (`StartServer`, `Raft`)
- Private: camelCase (`sendHeartbeat`)
- Interfaces: simple names with `-er` suffix (`Persister`, `Server`)

```go
type Raft struct {
	mu    sync.Mutex
	peers map[int]*peer
	state State  // Follower, Candidate, Leader
}
```

### Error Handling

- Always check and handle errors
- Return errors, never panic (unless truly unrecoverable)
- Use `fmt.Errorf` with `%w` for error wrapping
- Standardize error message format; never use `%v` when `%w` is meant

### Logging

- Use `log/slog` (stdlib structured logger). Prefer keyed attrs (`slog.Int("node", id)`, `slog.String("op", "vote")`).
- Levels: `Debug` for hot paths, `Info` for state changes, `Warn` for recoverable anomalies, `Error` for unrecoverable.
- Each `Raft` and `KVServer` owns a logger constructed via `slog.With("node", id)` so every log line self-labels its node.
- Format: `text` by default (`slog.NewTextHandler`); `-log.format=json` flag switches to JSON for production deploys.
- Level controlled by `-log.level` flag (debug/info/warn/error), default `info`.
- **No bare `log.Printf` in new code.**

### Concurrency Patterns

- Always use `sync.Mutex` for protecting shared state
- Pattern: `mu.Lock(); defer mu.Unlock()`
- **Never call RPCs while holding locks** (deadlock risk — the 0.0.x code hit this repeatedly)
- Spawn goroutines *after* releasing the lock; collect peer IDs under the lock, then unlock, then fan out
- Use channels for goroutine communication
- Use `context.Context` for cancellation and timeouts

## Testing

- Use table-driven tests for multiple test cases
- Test names: `Test<Area>_<Scenario>` with snake_case `t.Run` subtests (e.g. `TestElection_VoteRejected/already_voted`)
- **File split**:
  - `*_test.go` — external test package (`package raft_test`): blackbox scenario tests through the public API
  - `*_internal_test.go` — whitebox (`package raft`): direct asserts on private fields (`state`, `commitIndex`, `log`, `stepDown`)
- **Assertions**: use `github.com/stretchr/testify`. `require.X` for setup invariants / fatal assertions, `assert.X` for non-fatal. (testify is a direct dep.)
- For async conditions (leader elected, commit reached): `require.Eventually` / `require.Never`. **Do not use raw `time.Sleep`** — `Eventually` polls internally and is race-safe.
- Run all tests with `-race` flag
- Harness lives inline in `raft/test_harness_test.go` (whitebox `testCluster` + helpers). Partition tests get a Blocker interceptor later if needed — not built up front.
- Authority on test design references: `archive/test-harness-v1` branch (the 0.0.x prototype).

## gRPC Guidelines

- Define RPCs in `.proto` files; regenerate with `make proto` (never hand-edit `.pb.go`)
- Use `context.Context` as first parameter in RPC methods
- Each `*peer` stores a plain `*grpc.ClientConn`. Lazy dial via a ~20-line `ensureConnected` helper on `*peer`. No client wrapper struct.
- Server methods must be non-blocking; spawn goroutines for long operations
- Do **not** send verification RPCs with `Term: 0` during connect (confuses peers — see `archive/test-harness-v1` TODO #27). Peer dialing is lazy: dial on first RPC failure via `ensureConnected`, not eagerly at construction.
- Keepalive values (hardcoded in `server/`): EnforcementPolicy `{MinTime: 5s, PermitWithoutStream: true}`; ServerParameters `{Time: 10s, Timeout: 3s}`; ClientParameters mirror.
- `Raft` owns peer connections; `Shutdown()` closes them all. No separate connection pool.
- The `InstallSnapshot` RPC is declared in `raft.proto` from day one (for schema stability). The handler returns `codes.Unimplemented` until snapshotting is actually implemented.

## Raft Implementation Notes

- Read Raft paper sections 2-5, 7 before making changes
- Use randomized election timeouts (150-300ms range); the values come from yaml config (`cluster.election_timeout_min/max`)
- Persist state to disk **before** responding to any RPC (`currentTerm`, `votedFor`, `log`)
- Only commit log entries when majority has replicated
- Commit only entries from the *current* term (Figure 8 safety)
- Test with network partitions and node failures

### Goroutine model

- **Three goroutines** per node: election timer (always), heartbeat sender (leader-only), apply loop (always). Tracked by `sync.WaitGroup`.
- **Election timer**: `time.AfterFunc(d, becomeCandidate)` with `Reset(d)` on each AppendEntries/RequestVote received. No channel plumbing.
- **Synchronization**: `sync.Mutex` + channels. `applyCh chan ApplyMsg` (buffered 100) for the consumer; `commitCh chan struct{}` (buffered 1) for apply-loop wake signal. Dropped wakeups are harmless (applyLoop re-reads `commitIndex` under mutex). **No `sync.Cond` anywhere.**
- **Shutdown**: `context.WithCancel` from `Start()`. All goroutines select `<-ctx.Done()`. `Shutdown()` calls `cancel()` then `WaitGroup.Wait()`. Idempotent.

### Lifecycle invariants

- Constructor (`NewRaft`) does *not* start goroutines and does *not* dial peers. `Start()` starts goroutines; `Shutdown()` stops them.
- Production startup order (in `server/run.go`): load config → set up logger → bind listener → register services → `grpcServer.Serve()` (goroutine) → `rf.Start()` → block on signal. **Never dial peers before the local server is listening.**
- Production shutdown order: `rf.Shutdown()` → `kv.Kill()` → `grpcServer.GracefulStop()`.
- `kv.Kill()` spawns a drain goroutine on `applyCh` so `Raft`'s apply loop can exit before reaching `Shutdown`'s `WaitGroup.Wait()`.

### Identity & Persistence

- **Node identity is opaque.** IDs come from `cluster.yaml` (linear list under `nodes:`); binary matches itself via `-id` flag. **Never derive IDs from addresses/ports.** `deriveIdFromAddress` does not come back.
- `Persister` is an interface (`Save`/`Load` only for 0.1.0; snapshot methods added later as additive, non-breaking changes). Production impl is JSON-on-disk via atomic temp-file + `f.Sync()` + `os.Rename`. Tests may inject fault-injecting variants.
- **Always use `t.TempDir()` in tests** — never write to `./data` from a test.
- Log is a 0-indexed slice with a `logBase int` field declared now (always `0` in 0.1.0). All log access uses `log[absIndex - r.logBase - 1]`. `commitIndex`, `lastApplied`, `nextIndex`, `matchIndex` are absolute Raft indices. When snapshotting lands, only `Snapshot()` mutates `logBase`; every existing read site is already correct.
- `ApplyMsg` already carries both command and (zero-valued) snapshot fields; the type must not change when snapshotting lands later.

### Snapshotting (deferred — seams baked in for forward compatibility)

Snapshotting is **not implemented in 0.1.0**. The following seams are baked in so the eventual snapshotting work is additive (no refactors, no breaking changes):

- **`ApplyMsg` carries snapshot fields** (`SnapshotValid`, `SnapshotData`, `LastIncludedIndex`, `LastIncludedTerm`), zero-valued and unused in 0.1.0. The type must not change when snapshotting lands.
- **`logBase int` field on `Raft`** is declared now (always `0` in 0.1.0). All log access uses `log[absIndex - r.logBase - 1]`. When snapshotting lands, only `Snapshot()` mutates `logBase`; every existing read site is already correct.
- **`InstallSnapshot` RPC is declared in `raft.proto`** from day one. The handler returns `codes.Unimplemented` until the snapshot phase.
- **`cluster.snapshot_threshold` config field** (int, default `0` = disabled) is parsed but unused in 0.1.0.

The eventual snapshotting design:
- **Trigger**: KV server's `applyLoop` watches log size; when `len(state) > snapshot_threshold` (or applied entries past threshold exceed threshold), KV serializes its `map[string]string` and calls `rf.Snapshot(lastApplied, snapshotBytes)`. Raft truncates log up to `index`, persists the snapshot, bumps `logBase`. Raft never reads the state machine.
- **InstallSnapshot RPC** (leader → lagging follower): leader's `sendAppendEntries` checks `nextIndex[peer] - 1 < logBase`; if so, sends `InstallSnapshot` instead of `AppendEntries`. Single unary RPC, snapshot capped at ~4MB.
- **`CondInstallSnapshot`** (follower): only installs if `last_included_index > commitIndex` AND the snapshot's `last_included_term` matches the local log at that index (we have it). Otherwise discards (we've already applied past this point).
- **Persistence**: two files per node — `raft-state.json` (currentTerm, votedFor, log past logBase) and `snapshot.bin` (snapshot bytes + `{last_included_index, last_included_term}` header). Separate lifecycles: raft-state mutates every RPC; snapshot mutates rarely.
- **Recovery**: (1) load snapshot → restore `logBase`, send through `applyCh` so KV rehydrates state. (2) load raft-state → restore term/votedFor/log. (3) set `commitIndex = lastApplied = last_included_index` (no replay of snapshot-territory entries). (4) applyLoop applies subsequent entries normally as they commit.

Knob added to `cluster.yaml`:
```yaml
cluster:
  snapshot_threshold: 0    # entries; 0 = never snapshot
```

## KV Service Notes

- Op types: Get/Put/Append only for 0.1.0. Enum can extend later (additive).
- **Reads** go through the Raft log (provably linearizable, simple). ReadIndex is a later optimization.
- **Dedup**: `map[clientId]map[seqNum]*DuplicateEntry`, cap 100/client, TTL 10s. Known gap: dedup cache is *not* persisted, so a leader restart could re-execute a client retry. Fix is snapshot-territory. Document, don't patch around it.
- **`clientId`**: 8-byte `crypto/rand` int64. Never `time.Now().UnixNano()` (collision risk — see 0.0.x TODO #32).
- **Wrong-leader response**: `{wrong_leader bool, leader_id int32}`. Set `leaderId` only when this node is a **follower** (closes 0.0.x TODO #7 — was set unconditionally).
- **Pending ops**: `map[index]chan Result`, cap 1000, 5s timeout. Duplicates short-circuit to dedup cache without re-submitting.
- **Clerk** (in `kv/clerk.go`): lazy dial, 1000-attempt retry, exponential backoff 50ms→1s cap, panic at limit.

## Common Pitfalls

1. Deadlocks from holding locks during RPC calls
2. Not using `-race` flag in tests
3. Assuming linearizability without going through Raft (every read must go through the log for 0.1.0)
4. Forgetting to reset election timers on heartbeats (`AfterFunc.Reset(d)`)
5. Committing entries from a previous term without a current-term entry at the same index (Figure 8)
6. Not persisting state before responding to RPCs
7. Deriving node IDs from ports/addresses — use config ids, not parsing
8. Eagerly dialing peers at construction before the local gRPC server is bound
9. Writing test state to `./data` instead of `t.TempDir()`
10. Using `sync.Cond` instead of channels — channels + a buffered-1 wake signal is the canonical pattern here