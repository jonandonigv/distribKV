# distribKV Makefile
#
# Common targets:
#   make proto        regenerate all .pb.go from .proto
#   make build        build cmd/kvserver to bin/
#   make test         go test -race -cover ./...
#   make test-cover   open HTML coverage report
#   make run          run a 3-node cluster locally (background)
#   make stop         stop the local 3-node cluster started by `make run`
#   make smoke        exercise a Put/Append/Get sequence via a Clerk
#   make cluster-up   docker compose up -d --build (3 replicas)
#   make cluster-down   docker compose down
#   make cluster-logs docker compose logs -f
#   make fmt          gofmt -w .
#   make tidy         go mod tidy
#   make clean        rm -rf bin/ and coverage artifacts

PROTO_DIR     := proto
PROTO_FILES   := $(wildcard $(PROTO_DIR)/*.proto)
PROTO_GEN_GO  := protoc-gen-go
PROTO_GEN_RPC := protoc-gen-go-grpc
BIN_DIR       := bin
RUN_DIR       := .run
CONFIG        := configs/cluster.yaml
NODE_IDS      := 1 2 3

.PHONY: all proto build test test-cover run stop smoke cluster-up cluster-down cluster-logs fmt tidy clean help

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

## run: start a 3-node cluster locally in the background.
## Builds first, then launches bin/kvserver per node; logs land in .run/.
## Use `make stop` to stop them and `make smoke` to exercise the cluster.
run: build
	@mkdir -p $(RUN_DIR)
	@for id in $(NODE_IDS); do \
		port=$$(printf '1000%d' $$id); \
		nohup $(BIN_DIR)/kvserver -config $(CONFIG) -id $$id \
			> $(RUN_DIR)/node$$id.log 2>&1 & \
		echo $$! > $(RUN_DIR)/node$$id.pid; \
		echo "started node $$id (pid $$!) on :$$port, log .run/node$$id.log"; \
	done
	@echo "all nodes started. 'make stop' to stop, 'make smoke' to test."

## stop: stop the local 3-node cluster started by `make run`.
stop:
	@if [ -d $(RUN_DIR) ]; then \
		for pidfile in $(RUN_DIR)/*.pid; do \
			[ -f "$$pidfile" ] && kill $$(cat "$$pidfile") 2>/dev/null || true; \
		done; \
		sleep 1; \
		rm -rf $(RUN_DIR); \
		echo "cluster stopped"; \
	else echo "no $(RUN_DIR)/ dir — nothing to stop"; fi

## smoke: exercise a Put/Append/Get sequence against a running cluster.
## Assumes `make run` (local) or `make cluster-up` (docker) is already up.
smoke:
	go run ./cmd/smoke -config $(CONFIG)

## cluster-up: build and start the 3-node docker cluster in the background.
cluster-up:
	docker compose up -d --build

## cluster-down: stop and remove the docker cluster (volumes preserved).
cluster-down:
	docker compose down

## cluster-logs: tail the docker cluster logs.
cluster-logs:
	docker compose logs -f

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' | column -t -s ':'