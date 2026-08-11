// Package server wires the consensus and KV layers into a runnable
// production node: it builds the gRPC server with the project's
// keepalive policy, registers the raft and kv services, and drives the
// lifecycle (start/serve/shutdown) per AGENTS.md "Lifecycle invariants".
//
// The binary that imports this package is cmd/kvserver.

package server

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// keepalive policy constants from AGENTS.md "gRPC Guidelines".
const (
	keepaliveMinTime             = 5 * time.Second
	keepalivePermitWithoutStream = true
	keepaliveTime                = 10 * time.Second
	keepaliveTimeout             = 3 * time.Second
)

// newGRPCServer builds a *grpc.Server configured with the project's
// canonical keepalive policy (AGENTS.md "gRPC Guidelines"):
//
//	EnforcementPolicy: MinTime 5s, PermitWithoutStream true
//	ServerParameters:   Time 10s, Timeout 3s
//
// Insecure transport credentials are used for 0.1.0 (portfolio scope);
// TLS is a later additive concern.
func newGRPCServer() *grpc.Server {
	return grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinTime,
			PermitWithoutStream: keepalivePermitWithoutStream,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    keepaliveTime,
			Timeout: keepaliveTimeout,
		}),
	)
}
