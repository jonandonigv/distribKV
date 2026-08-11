// Command smoke is the distribKV operator sanity-check tool. It loads a
// cluster.yaml, builds a Clerk over the configured node addresses, runs
// a Put -> Append -> Get sequence against the cluster, and exits 0 on
// success or 1 on any failure. It assumes a cluster is already running
// (started via `make run` for local processes or `make cluster-up` for
// docker); smoke does NOT start the cluster itself.
//
// Usage:
//
//	go run ./cmd/smoke -config configs/cluster.yaml
//
// Exit codes: 0 = cluster healthy and KV ops round-trip; 1 = any error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jonandonigv/distribKV/config"
	"github.com/jonandonigv/distribKV/kv"
)

func main() {
	cfgPath := flag.String("config", "configs/cluster.yaml", "path to cluster.yaml")
	timeout := flag.Duration("timeout", 30*time.Second, "total smoke timeout")
	flag.Parse()

	if err := run(*cfgPath, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("smoke: OK")
}

// run loads the config, builds a Clerk, and exercises a Put->Append->Get
// sequence. It blocks until a leader is reachable and the ops succeed,
// bounded by timeout. Returns nil on success.
func run(cfgPath string, timeout time.Duration) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	addrs := make([]string, 0, len(cfg.Nodes))
	ids := make([]int, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		addrs = append(addrs, cfg.Nodes[i].ListenAddr)
		ids = append(ids, cfg.Nodes[i].ID)
	}

	// Retry the whole sequence until the cluster is up and the ops
	// succeed, bounded by timeout. The Clerk itself retries internally
	// (MaxClerkAttempts) but we wrap it so a cold start (leader not yet
	// elected) doesn't fail instantly.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for {
		err := tryOnce(addrs, ids)
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("smoke did not succeed within %s: %w", timeout, lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// tryOnce builds a Clerk, runs the Put->Append->Get sequence, and asserts
// the final value. The Clerk (per AGENTS.md) panics after MaxClerkAttempts
// (1000) — we recover that panic and surface it as an error so the outer
// retry loop in run() can keep trying during a cold start (leader not yet
// elected). Returns nil on success.
func tryOnce(addrs []string, ids []int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("clerk exhausted retries: %v", r)
		}
	}()

	key := "smoke-check"
	want := "hello-world"

	ck := kv.MakeClerk(addrs, ids, nil)
	defer ck.CloseConn()

	ck.Put(key, "hello")
	ck.Append(key, "-world")
	got := ck.Get(key)
	if got != want {
		return fmt.Errorf("Get(%q) = %q, want %q", key, got, want)
	}
	return nil
}
