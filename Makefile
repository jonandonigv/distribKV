.PHONY: build server client test clean fmt lint help

help:
	@echo "Available targets:"
	@echo "  build   - Build server and client binaries"
	@echo "  server  - Build server binary only"
	@echo "  client  - Build client binary only"
	@echo "  test    - Run tests with race detector"
	@echo "  clean   - Remove binaries"
	@echo "  fmt     - Format code with gofmt"
	@echo "  lint    - Run golangci-lint (if installed)"
	@echo "  help    - Show this help message"

build: server client

server:
	go build -o bin/grpc-test-server ./cmd/grpc-test-server

client:
	go build -o bin/grpc-test-client ./cmd/grpc-test-client

test:
	go test -race ./...

clean:
	rm -rf bin/

fmt:
	gofmt -w .

lint:
	golangci-lint run ./... || true
