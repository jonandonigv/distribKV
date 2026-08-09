# distribKV Makefile
#
# Common targets:
#   make proto        regenerate all .pb.go from .proto
#   make build        build cmd/kvserver to bin/
#   make test         go test -race -cover ./...
#   make test-cover   open HTML coverage report
#   make run          run a 3-node cluster locally (step 8)
#   make cluster-up   docker-compose up -d (step 8)
#   make fmt          gofmt -w .
#   make tidy         go mod tidy
#   make clean        rm -rf bin/

PROTO_DIR     := proto
PROTO_FILES   := $(wildcard $(PROTO_DIR)/*.proto)
PROTO_GEN_GO  := protoc-gen-go
PROTO_GEN_RPC := protoc-gen-go-grpc
BIN_DIR       := bin

.PHONY: all proto build test test-cover run cluster-up cluster-down cluster-logs smoke fmt tidy clean help

all: tidy build

## proto: regenerate all .pb.go from .proto (never hand-edit .pb.go)
proto:
	@which $(PROTO_GEN_GO)  >/dev/null 2>&1 || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@which $(PROTO_GEN_RPC) >/dev/null 2>&1 || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	protoc \
		--go_out=. --go_opt=module=github.com/jonandonigv/distribKV \
		--go-grpc_out=. --go-grpc_opt=module=github.com/jonandonigv/distribKV \
		$(PROTO_FILES)
	@echo "regenerated: $(PROTO_FILES)"

## build: build cmd/kvserver to bin/
build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/kvserver ./cmd/kvserver

## test: go test -race -cover ./...
test:
	go test -race -cover ./...

## test-cover: open HTML coverage report
test-cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

## fmt: gofmt -w .
fmt:
	gofmt -w .

## tidy: go mod tidy
tidy:
	go mod tidy

## clean: rm -rf bin/ and coverage artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out

## run / cluster-* / smoke: stubs for step 8 (deployment)
run:
	@echo "make run is a stub — implemented in step 8"

cluster-up cluster-down cluster-logs smoke:
	@echo "$@ is a stub — implemented in step 8"

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | column -t -s ':'