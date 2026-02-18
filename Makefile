.PHONY: all build kvserver server client kv-client test clean fmt lint tidy help

help:
	@echo "Available targets:"
	@echo "  all        - Run tidy and build all binaries"
	@echo "  build      - Build all binaries (kvserver, kv-client, grpc-test-server, grpc-test-client)"
	@echo "  kvserver   - Build kvserver binary (Raft KV store)"
	@echo "  kv-client  - Build kv-client binary (KV cluster test client)"
	@echo "  server     - Build gRPC test server binary"
	@echo "  client     - Build gRPC test client binary"
	@echo "  test       - Run all tests with race detector"
	@echo "  test-v     - Run tests with verbose output"
	@echo "  test-raft  - Run Raft package tests with verbose output"
	@echo "  tidy       - Run go mod tidy"
	@echo "  clean      - Remove binaries"
	@echo "  fmt        - Format code with gofmt"
	@echo "  lint       - Run golangci-lint (if installed)"
	@echo "  help       - Show this help message"

all: tidy build

build: kvserver kv-client server client

kvserver:
	go build -o bin/kvserver ./cmd/kvserver

kv-client:
	go build -o bin/kv-client ./cmd/kv-client

server:
	go build -o bin/grpc-test-server ./cmd/grpc-test-server

client:
	go build -o bin/grpc-test-client ./cmd/grpc-test-client

test:
	go test -race ./...

test-v:
	go test -race -v ./...

test-raft:
	go test -race ./pkg/raft -v

tidy:
	go mod tidy

clean:
	rm -rf bin/

fmt:
	gofmt -w .

lint:
	golangci-lint run ./... || true
