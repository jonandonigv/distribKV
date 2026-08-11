# syntax=docker/dockerfile:1
#
# Multi-stage build for the distribKV kvserver binary.
# Builder: golang image compiles a static binary (CGO disabled).
# Runtime: gcr.io/distroless/static-debian12 (no shell, no package
# manager) running as non-root. The configs/ directory is copied in so
# the compose stack can reference configs/cluster-docker.yaml.

# --- stage 1: build ---
FROM golang:1.25 AS builder

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source and build the single binary. CGO is
# disabled so the resulting binary is fully static and can run in the
# distroless/static image.
COPY . .
RUN CGO_ENABLED=0 go build -o /out/kvserver ./cmd/kvserver

# --- stage 2: runtime ---
# distroless/static is a minimal base with no shell or package manager
# (~2MB). It is suitable for pure-Go static binaries like ours. A
# nonroot user (UID 65532) is built in.
FROM gcr.io/distroless/static-debian12

WORKDIR /

# Copy the binary and the bundled configs (cluster.yaml for local refs
# and cluster-docker.yaml for the compose stack).
COPY --from=builder /out/kvserver /bin/kvserver
COPY configs/ /configs/

# Run as the distroless non-root user.
USER nonroot

# The canonical 3-node cluster listens on 10001-10003.
EXPOSE 10001 10002 10003

ENTRYPOINT ["/bin/kvserver"]