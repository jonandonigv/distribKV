// Blackbox tests for the Health service implementation. Covers Check
// (returns SERVING), Watch (returns codes.Unimplemented), and the
// gRPC server registration path.

package health_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/health"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// startHealth spins a grpc server with the Health service on an
// ephemeral port and returns the client + a cleanup func.
func startHealth(t *testing.T) (health.HealthClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	health.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	return health.NewHealthClient(conn), func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
}

func TestCheck_ReturnsServing(t *testing.T) {
	c, cleanup := startHealth(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.Check(ctx, &health.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, health.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestCheck_IgnoresServiceName(t *testing.T) {
	// The optional service field is ignored in 0.1.0; any value still
	// returns SERVING.
	c, cleanup := startHealth(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.Check(ctx, &health.HealthCheckRequest{Service: "anything"})
	require.NoError(t, err)
	assert.Equal(t, health.HealthCheckResponse_SERVING, resp.GetStatus())
}

func TestWatch_ReturnsUnimplemented(t *testing.T) {
	c, cleanup := startHealth(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := c.Watch(ctx, &health.HealthCheckRequest{})
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "expected a grpc status error")
	assert.Equal(t, codes.Unimplemented, st.Code())
}

func TestNewServer_NotNil(t *testing.T) {
	s := health.NewServer()
	require.NotNil(t, s)
}
