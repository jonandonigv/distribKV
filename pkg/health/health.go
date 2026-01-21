package health

import (
	"context"

	healthpb "github.com/jonandonigv/distribKV/proto"
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
	return nil
}
