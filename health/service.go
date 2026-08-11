// Package health implements the per-node Health gRPC service declared in
// proto/health.proto. The 0.1.0/0.1.1 implementation is intentionally
// trivial: Check always returns SERVING once the gRPC server is accepting
// connections, and Watch is declared for schema stability but returns
// codes.Unimplemented.
//
// A future status-aware variant could gate on rf.Start() success or raft
// leadership, but the self-healthcheck approach (see AGENTS.md) keeps 0.1.0
// simple: the docker-compose healthcheck and the `kvserver healthcheck`
// subcommand only need to know the node is up and serving RPCs.

package health

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements the Health service. Embed UnimplementedHealthServer
// for forward compatibility (new RPCs added to the proto return
// Unimplemented automatically).
type Server struct {
	UnimplementedHealthServer
}

// NewServer returns a Health server that reports SERVING on Check.
func NewServer() *Server {
	return &Server{}
}

// Check reports the node's serving status. 0.1.0 always returns SERVING;
// the optional service name in the request is ignored.
func (s *Server) Check(ctx context.Context, req *HealthCheckRequest) (*HealthCheckResponse, error) {
	return &HealthCheckResponse{Status: HealthCheckResponse_SERVING}, nil
}

// Watch streams serving-status updates. Declared in the proto for schema
// stability; not implemented in 0.1.0. Returns codes.Unimplemented so any
// caller (e.g. grpc-health-probe) fails cleanly rather than hanging.
func (s *Server) Watch(req *HealthCheckRequest, stream Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented in 0.1.0")
}
