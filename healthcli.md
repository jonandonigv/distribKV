# gRPC Health CLI Testing Tool

## Overview

Build separate binaries (`grpc-test-server` and `grpc-test-client`) using standard library flags to test gRPC layers with a standard Health Check service.

---

## Phase 1: Protocol Buffer Definition

### Files to create:
- `proto/health.proto`

### Content:

```protobuf
syntax = "proto3";

package health;

option go_package = "github.com/jonandonigv/distribKV/proto/health";

service Health {
  rpc Check(HealthCheckRequest) returns (HealthCheckResponse);
  rpc Watch(HealthCheckRequest) returns (stream HealthCheckResponse);
}

message HealthCheckRequest {
  string service = 1;
}

message HealthCheckResponse {
  enum ServingStatus {
    UNKNOWN = 0;
    SERVING = 1;
    NOT_SERVING = 2;
    SERVICE_UNKNOWN = 3;
  }
  ServingStatus status = 1;
}
```

### Rationale:

Standard GRPC Health Checking Protocol - used by Kubernetes, gRPC ecosystem, and future Raft health monitoring.

---

## Phase 2: Generate gRPC Code

### Actions:

1. Install protoc and Go plugins if not present
2. Generate code:
   ```bash
   protoc --go_out=. --go_opt=paths=source_relative \
          --go-grpc_out=. --go-grpc_opt=paths=source_relative \
          proto/health.proto
   ```

### Dependencies to add to `go.mod`:

- `google.golang.org/grpc` (already present)
- `google.golang.org/protobuf` (already present)

### Generated files:

- `proto/health/health.pb.go`
- `proto/health/health_grpc.pb.go`

---

## Phase 3: Health Service Implementation

### File to create: `pkg/health/health.go`

### Structure:

```go
package health

import (
	"context"

	healthpb "github.com/jonandonigv/distribKV/proto/health"
)

type Server struct {
	healthpb.UnimplementedHealthServer
}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_SERVING,
	}, nil
}

func (s *Server) Watch(req *healthpb.HealthCheckRequest, stream healthpb.Health_WatchServer) error {
	// Streaming implementation for future use
	return nil
}
```

### Rationale:

Simple, testable implementation that always returns SERVING - ideal for verifying gRPC connectivity works.

---

## Phase 4: Server CLI Binary

### Directory to create: `cmd/grpc-test-server/`

### File to create: `cmd/grpc-test-server/main.go`

### CLI Flags:

```go
var (
	port     = flag.Int("port", 50051, "Server port")
	serverID = flag.String("id", "1", "Server identifier for logging")
)
```

### Implementation:

1. Parse flags
2. Create `common.NewServer(fmt.Sprintf(":%d", *port))`
3. Call `server.Start()`
4. Create `health.NewServer()`
5. Register service: `server.RegisterService(&healthpb.Health_ServiceDesc, healthSrv)`
6. Setup graceful shutdown with `os.Signal` handling (SIGINT, SIGTERM)
7. Log connections and requests

### Features:

- Logs server startup with port and ID
- Tracks number of health check requests received
- Graceful shutdown on Ctrl+C
- Returns error on port conflicts

---

## Phase 5: Client CLI Binary

### Directory to create: `cmd/grpc-test-client/`

### File to create: `cmd/grpc-test-client/main.go`

### CLI Flags:

```go
var (
	serverAddr = flag.String("addr", "localhost:50051", "Server address")
	mode       = flag.String("mode", "single", "Mode: 'single' or 'continuous'")
	numReqs    = flag.Int("requests", 1, "Number of requests (continuous mode)")
	delay      = flag.Duration("delay", 1*time.Second, "Delay between requests (continuous mode)")
	clientID   = flag.String("id", "client", "Client identifier for logging")
)
```

### Implementation:

#### Single Mode:

1. Create `common.NewClient(*serverAddr)`
2. Connect with timeout context
3. Create Health client from generated code
4. Call `Check()` RPC
5. Log response status and latency
6. Close connection
7. Exit with status 0 on success, 1 on failure

#### Continuous Mode:

1. Same as single mode, but:
2. Loop for `*numReqs` iterations
3. Add `*delay` between requests
4. Track success/failure count
5. Handle context cancellation
6. Log statistics at end (success rate, avg latency)

### Error Handling:

- Use `common.WithRetry` for connection with maxRetries=3
- Handle `context.DeadlineExceeded` explicitly
- Log gRPC status codes for detailed error info

### Features:

- Configurable server address and port
- Support for single and continuous request modes
- Success/failure statistics
- Latency tracking
- Clean shutdown on Ctrl+C

---

## Phase 6: Build Configuration

### File to update: `AGENTS.md`

Add build commands:

```bash
# Build server
go build -o bin/grpc-test-server ./cmd/grpc-test-server

# Build client
go build -o bin/grpc-test-client ./cmd/grpc-test-client

# Build both
go build -o bin/ ./cmd/...
```

### File to create: `Makefile` (optional, for convenience)

```makefile
.PHONY: build server client test clean

build: server client

server:
	go build -o bin/grpc-test-server ./cmd/grpc-test-server

client:
	go build -o bin/grpc-test-client ./cmd/grpc-test-client

test:
	go test ./...

clean:
	rm -rf bin/
```

---

## Phase 7: Documentation

### File to update: `README.md`

Add section: **Testing gRPC Infrastructure**

```markdown
## Testing gRPC Infrastructure

### Start Server

```bash
# Start on default port (50051)
./bin/grpc-test-server

# Start on custom port
./bin/grpc-test-server --port=10001 --id=server-1
```

### Run Client

**Single Request Mode (default):**
```bash
./bin/grpc-test-client --addr=localhost:50051 --id=client-1
```

**Continuous Request Mode:**
```bash
# Send 10 requests with 500ms delay
./bin/grpc-test-client --addr=localhost:50051 --mode=continuous --requests=10 --delay=500ms --id=load-test-1
```

### Multi-Client Testing

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
```

---

## Phase 8: Testing Checklist

### Manual testing steps:

1. Build binaries
2. Start server: `./bin/grpc-test-server --port=50051`
3. Test single request: `./bin/grpc-test-client --addr=localhost:50051 --id=test`
4. Verify SERVING response and latency logging
5. Test continuous mode: `./bin/grpc-test-client --mode=continuous --requests=10 --delay=1s`
6. Verify statistics output
7. Test multiple clients in parallel (different terminals)
8. Test graceful shutdown: Ctrl+C on server
9. Test connection failure: Start client without server
10. Test WithRetry: Verify retry on connection failure

### Expected outputs:

- Server logs connection from each client
- Server logs number of requests received
- Client logs request/response status
- Client logs latency in milliseconds
- Client logs success rate in continuous mode
- Clean shutdown without data loss

---

## Directory Structure After Implementation

```
distribKV/
├── cmd/
│   ├── grpc-test-server/
│   │   └── main.go
│   └── grpc-test-client/
│       └── main.go
├── pkg/
│   ├── common/
│   │   └── grpc.go
│   └── health/
│       └── health.go
├── proto/
│   ├── health.proto
│   └── health/
│       ├── health.pb.go
│       └── health_grpc.pb.go
├── bin/
│   ├── grpc-test-server
│   └── grpc-test-client
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

---

## Success Criteria

- [ ] Server binary starts and listens on specified port
- [ ] Client binary connects to server and receives health check responses
- [ ] Multiple clients can connect simultaneously
- [ ] Continuous mode works with configurable request count and delay
- [ ] Graceful shutdown works on both server and client
- [ ] Proper error handling for connection failures
- [ ] Statistics logged (success rate, latency)
- [ ] `go test` passes for all packages
- [ ] Documentation is clear and accurate

---

## Design Decisions

### Why Standard Health Check Service?

- Industry-standard protocol (gRPC Health Checking Protocol v1)
- Used by Kubernetes for readiness/liveness probes
- Compatible with existing monitoring tools
- Simple to implement and test
- Foundation for future Raft cluster health monitoring

### Why Separate Binaries?

- Clearer separation of concerns
- Simpler deployment (can run server instances independently)
- Easier to test in isolation
- Follows Go CLI conventions
- Simpler code (no subcommand routing logic)

### Why Standard Library Flags?

- No additional dependencies
- Sufficient for this use case
- Familiar to Go developers
- Simpler build process
- Consistent with project's philosophy

### Why Configurable Client Mode?

- Supports both quick smoke tests and load testing
- Demonstrates gRPC connection reuse
- Tests behavior under load
- Future-proof for more complex scenarios
- Provides flexibility for different testing needs

---

## Next Steps

To implement this plan, follow these phases in order:

1. Create `proto/health.proto` with service definition
2. Generate gRPC code using protoc
3. Implement `pkg/health/health.go` service
4. Create `cmd/grpc-test-server/main.go` binary
5. Create `cmd/grpc-test-client/main.go` binary
6. Test with manual checklist
7. Update documentation in README.md
8. Add to AGENTS.md build commands
