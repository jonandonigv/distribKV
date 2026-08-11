# distribKV

> A portfolio project: building a distributed key-value store on top of the Raft consensus algorithm, from scratch in Go.

## Why this project

Most backend work hides the hard parts of distributed systems behind a managed database. This project surfaces them. The goal is to understand — by implementing — how systems like etcd, Consul, and TiKV stay consistent and available when networks fail, clocks drift, and nodes crash.

It is built as a learning-focused portfolio piece following the [MIT 6.824 Distributed Systems](https://pdos.csail.mit.edu/6.824/) progression, with production-grade engineering practices on top: table-driven tests, the `-race` detector, atomic persistence, clean shutdown semantics, and config-driven deployment.

## What it demonstrates

- **Consensus algorithms** — leader election, log replication, and the safety invariants of Raft
- **Fault tolerance** — crash recovery via persistent state, partition handling, automatic re-election
- **Strong consistency** — linearizable reads and writes through a replicated log
- **Go concurrency** — goroutines, channels, `sync.Mutex`, `context.Context`, the race detector
- **gRPC / Protocol Buffers** — typed RPCs, keepalive, health checks
- **Systems design** — clean lifecycle (startup, shutdown, restart), configuration, deployment

## Architecture

![distribKV Architecture](Architecture.png)

### Two-layer design

**1. Raft consensus layer** (`raft/`)

- Leader election with randomized timeouts (150–300ms from config)
- Log replication with conflict detection and `nextIndex`/`matchIndex` tracking
- State persistence (atomic temp-file + `f.Sync()` + `os.Rename`)
- Three background goroutines per node (election timer, heartbeat sender, apply loop), cleanly shut down via `context.Context` cancellation
- `ApplyMsg` channel for state-machine integration

**2. Key-value service layer** (`kv/`)

- `Get`/`Put`/`Append` operations through Raft (every read goes through the log — provably linearizable)
- Duplicate detection (`clientId`/`seqNum` cache, cap 100/client, 10s TTL)
- Wrong-leader hints (`{wrong_leader, leader_id}`) so clients fail over fast
- Pending-op tracking with 5s timeout and `ErrTooManyPending` backpressure
- `Clerk` client library: lazy dial, 1000-attempt retry, exponential backoff 50ms→1s cap

### Configuration-driven identity

Node IDs are opaque integers from `cluster.yaml` — never derived from ports. Each node matches itself by `-id` and learns its peers from the same file. No `deriveIdFromAddress`, no port-coupling, no surprises on ephemeral test ports.

### Forward-compatible with snapshotting

Log compaction (snapshotting) isn't implemented yet, but the seams are baked in so the eventual work is additive: `ApplyMsg` already carries snapshot fields (zero-valued), `logBase` is declared and used in every log access, `raft.proto` declares `InstallSnapshot` (handler returns `codes.Unimplemented`), and `cluster.yaml` carries a `snapshot_threshold` knob (default `0` = disabled).

## Project structure

```
distribKV/
├── cmd/
│   ├── kvserver/      # single binary (server + healthcheck subcommand)
│   └── smoke/         # operator sanity-check tool (make smoke)
├── config/            # cluster.yaml parsing
├── raft/              # consensus engine; *_test.go files include the test harness
├── kv/                # KV state machine + Clerk client library
├── server/            # binary wiring (run.go, grpc.go)
├── health/            # Health gRPC service impl
├── proto/             # .proto source files (raft, kv, health)
├── configs/
│   ├── cluster.yaml         # canonical local-dev cluster (./data, 0.0.0.0 ports)
│   └── cluster-docker.yaml  # docker stack (service-name addrs, /var/lib data)
├── docker-compose.yml # 3 kvserver replicas with healthchecks
├── Dockerfile         # multi-stage Go builder; distroless/static runtime
├── Makefile           # proto/build/test/run/stop/smoke/cluster-* targets
└── ...reference docs (project.md, Architecture.png, CONTRIBUTING.md, AGENTS.md, Notes/)
```

Generated `.pb.go` files live in the consuming package so code references proto types without a `pb.` import indirection. Raft uses a `raft/raftpb/` subpackage because its proto `LogEntry` embeds a mutex that trips `go vet` on slice iteration; `kv` and `health` keep theirs flat in the package.

## Build & test

The `proto`/`build`/`test`/`test-cover`/`fmt`/`tidy`/`run`/`stop`/`smoke`/`cluster-*` targets are all live:

```bash
make proto              # regenerate all .pb.go from .proto (never hand-edit .pb.go)
make build              # build ./cmd/kvserver to bin/
make test               # go test -race -cover ./...
make test-cover         # open HTML coverage report

# Local 3-node cluster
make run                # 3 background processes (logs in .run/)
make smoke              # Put/Append/Get via Clerk against the running cluster
make stop               # stop the background processes

# Docker
make cluster-up         # docker compose up -d --build
make cluster-down       # docker compose down
make cluster-logs       # docker compose logs -f
```

Tests use `github.com/stretchr/testify` (`require` for fatal assertions, `assert` for non-fatal, `require.Eventually`/`Never` for async conditions — no `time.Sleep`). The race detector is mandatory.

## Cluster configuration

```yaml
cluster:
  name: distribkv-dev
  heartbeat_interval: 50ms
  election_timeout_min: 150ms
  election_timeout_max: 300ms
  data_dir: ./data             # local dev (gitignored); docker uses /var/lib/distribkv
  snapshot_threshold: 0    # entries; 0 = never snapshot (deferred)

nodes:
  - id: 1
    listen_addr: "0.0.0.0:10001"
  - id: 2
    listen_addr: "0.0.0.0:10002"
  - id: 3
    listen_addr: "0.0.0.0:10003"
```

Run a node with: `bin/kvserver -config configs/cluster.yaml -id 1 -log.level info -log.format text`

## Resources

- [Raft Paper](https://raft.github.io/raft.pdf) — the consensus algorithm foundation
- [Raft Visualization](https://raft.github.io/) — interactive algorithm demo
- [MIT 6.824 Distributed Systems](https://pdos.csail.mit.edu/6.824/) — course inspiration

---

_Built as a learning project following MIT 6.824, architected with production patterns in mind._