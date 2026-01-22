# AGENTS.md

This file contains instructions for agentic coding assistants working on this repository.

## Project Overview

Distributed Key-Value Store using Raft consensus algorithm in Go. This is a learning-focused implementation following MIT 6.824 Distributed Systems course.

## Build, Lint, and Test Commands

```bash
go mod tidy
go build -o bin/kvserver ./cmd/raft-kv-server

# gRPC Health CLI Tools
go build -o bin/grpc-test-server ./cmd/grpc-test-server
go build -o bin/grpc-test-client ./cmd/grpc-test-client
go build -o bin/ ./cmd/...

# Testing
go test ./...
go test -race ./...
go test ./pkg/raft -run TestElection -v
go test -race ./pkg/raft
go test -race -cover ./...
```

## Code Style Guidelines

### Imports
- Group imports: stdlib, third-party, project (blank line between)
- Sort alphabetically within each group

```go
import (
	"context"
	"sync"

	"google.golang.org/grpc"

	"github.com/jonandonigv/distribKV/pkg/raft"
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

### Naming Conventions
- Package names: lowercase, single word (`common`, `raft`)
- Public: PascalCase (`StartServer`, `Raft`)
- Private: camelCase (`sendHeartbeat`)
- Interfaces: simple names with `-er` suffix (`Persister`, `Server`)

```go
type Raft struct {
    mu    sync.Mutex
    peers []*grpc.ClientConn
    state State  // Follower, Candidate, Leader
}
```

### Error Handling
- Always check and handle errors
- Return errors, never panic (unless truly unrecoverable)
- Use `fmt.Errorf` with `%w` for error wrapping

```go
func (c *Client) Connect(ctx context.Context) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    conn, err := grpc.DialContext(ctx, c.target, grpc.WithBlock())
    if err != nil {
        return fmt.Errorf("failed to dial %s: %w", c.target, err)
    }
    c.conn = conn
    return nil
}
```

### Concurrency Patterns
- Always use `sync.Mutex` for protecting shared state
- Pattern: `mu.Lock(); defer mu.Unlock()`
- Never call RPCs while holding locks (deadlock risk)
- Use channels for goroutine communication
- Use `context.Context` for cancellation and timeouts

### Testing
- Use table-driven tests for multiple test cases
- Test files: `filename_test.go` in same package
- Test names: `TestFunctionName` or `TestScenario_Description`
- Use `t.Fatalf` for setup failures, `t.Errorf` for assertions
- Run all tests with `-race` flag
- Prefer channels/sync.Cond over `time.Sleep`

```go
func TestRequestVote(t *testing.T) {
    tests := []struct {
        name string
        args RequestVoteArgs
        want RequestVoteReply
    }{
        {"vote granted", RequestVoteArgs{term: 1}, RequestVoteReply{voteGranted: true}},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            reply := handleRequestVote(tt.args)
            if reply.voteGranted != tt.want.voteGranted {
                t.Errorf("got %v, want %v", reply.voteGranted, tt.want.voteGranted)
            }
        })
    }
}
```

## gRPC Guidelines
- Define RPCs in `.proto` files, generate with `protoc`
- Use `context.Context` as first parameter in RPC methods
- Use `grpc.ClientConn` for client connections (reuse)
- Server methods should be non-blocking; use goroutines for long operations

## Raft Implementation Notes
- Read Raft paper sections 2-5, 7 before making changes
- Use randomized election timeouts (150-300ms range)
- Persist state to disk before responding to RPCs
- Only commit log entries when majority has replicated
- Test with network partitions and node failures

## Common Pitfalls
1. Deadlocks from holding locks during RPC calls
2. Not using `-race` flag in tests
3. Assuming linearizability without going through Raft
4. Forgetting to reset election timers on heartbeats
5. Committing from previous term without majority
6. Not persisting state before responding to RPCs
