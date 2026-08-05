package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestShutdown_CompletesQuickly verifies that Shutdown() returns within a
// bounded interval, proving the election timer, apply goroutine, and (if this
// node is leader) the heartbeat sender have all observed the stop signal.
// Before Shutdown() existed these loops ran forever and leaked across tests.
func TestShutdown_CompletesQuickly(t *testing.T) {
	tc := newTestCluster(t, 3)
	tc.start()

	// Elect a leader so the heartbeat-sender goroutine is also running on one
	// node and gets exercised by Shutdown(). We only need *a* leader to
	// exist, not long-term stability.
	leader := tc.requireLeader(5 * time.Second)
	require.NotNil(t, leader)

	done := make(chan struct{})
	go func() {
		tc.shutdown()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown() did not complete within 5s; goroutines leaked")
	}
}

// TestShutdown_Idempotent asserts that calling Shutdown() more than once does
// not panic (double-close was a real risk in the original select-based stopper).
func TestShutdown_Idempotent(t *testing.T) {
	tc := newTestCluster(t, 3)
	tc.start()
	tc.shutdown()

	for _, rf := range tc.rafts {
		require.NotPanics(t, func() { rf.Shutdown() })
	}
}

// TestShutdown_WithoutStart verifies Shutdown is safe even on a node that was
// constructed but never had Start() called (only the apply goroutine is
// running in that case).
func TestShutdown_WithoutStart(t *testing.T) {
	tc := newTestCluster(t, 3)
	defer tc.shutdown()
	// No tc.start() — exercise the construct-without-start path.
	done := make(chan struct{})
	go func() {
		for _, rf := range tc.rafts {
			rf.Shutdown()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown() on unstarted node hung")
	}
}
