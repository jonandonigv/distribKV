package common

import (
	"context"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Server struct {
	addr string
	srv  *grpc.Server
	mu   sync.Mutex
}

func NewServer(addr string) *Server {
	return &Server{addr: addr}
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv != nil {
		return nil
	}

	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	s.srv = grpc.NewServer()

	go func() {
		log.Printf("gRPC server listening on %s", s.addr)
		if err := s.srv.Serve(lis); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	return nil
}

func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv == nil {
		log.Fatal("server not started")
	}

	s.srv.RegisterService(desc, impl)
}

func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.srv != nil {
		s.srv.GracefulStop()
		s.srv = nil
	}
}

type Client struct {
	conn   *grpc.ClientConn
	target string
	mu     sync.RWMutex
}

func NewClient(target string) *Client {
	return &Client{target: target}
}

func (c *Client) Connect(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	conn, err := grpc.NewClient(
		c.target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}

	c.conn = conn
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}

	return nil
}

func (c *Client) Conn() *grpc.ClientConn {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.conn
}

func GetLocalAddr() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "localhost"
	}

	addrs, err := net.LookupHost(hostname)
	if err != nil || len(addrs) == 0 {
		return "localhost"
	}

	return addrs[0] + ":50051"
}

func WithRetry(ctx context.Context, maxRetries int, delay time.Duration, fn func() error) error {
	var lastErr error

	for range maxRetries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return lastErr
}
