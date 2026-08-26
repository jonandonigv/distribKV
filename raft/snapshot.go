// Local snapshotting: Raft.Snapshot truncates the log up to a caller-
// supplied index (the KV layer's lastApplied), stores the opaque
// state-machine snapshot bytes, and persists both files. Recovery
// (loadPersistedState) restores logBase and re-delivers the snapshot
// through applyCh so the KV layer rebuilds its state — see PLAN.md
// "Step S1" and AGENTS.md "Snapshotting".
//
// Snapshotting does NOT change what is committed or applied: entries up
// to the snapshot index were already applied by the caller, so no
// in-flight application can race with the truncation.

package raft

import (
	"fmt"
	"log/slog"
)

// Snapshot compacts the log: all entries with Index <= index are folded
// into the snapshot. The caller must guarantee it has applied every entry
// through `index` (KV passes its lastApplied) — raft trusts but verifies:
//   - index > commitIndex → error (uncommitted territory).
//   - index <= logBase    → nil, no-op (already covered).
//   - entry missing at index → error (caller out of sync with the log).
//
// Both persistence files are rewritten before returning: raft-state.json
// now holds only the log tail past logBase; snapshot.bin holds the bytes
// + {index, term}. data may be empty only if the state machine truly has
// no state; raft treats the bytes as opaque.
func (r *Raft) Snapshot(index int, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if index > r.commitIndex {
		return fmt.Errorf("snapshot index %d beyond commitIndex %d", index, r.commitIndex)
	}
	if index <= r.logBase {
		return nil // already covered by an earlier snapshot
	}

	arrayIdx := index - r.logBase - 1
	if arrayIdx < 0 || arrayIdx >= len(r.log) || int(r.log[arrayIdx].Index) != index {
		return fmt.Errorf("log does not contain entry at index %d (logBase %d, len %d)",
			index, r.logBase, len(r.log))
	}
	snapTerm := int(r.log[arrayIdx].Term)

	// Keep the tail strictly after the snapshot point.
	tail := make([]LogEntry, 0, len(r.log)-arrayIdx-1)
	tail = append(tail, r.log[arrayIdx+1:]...)
	r.log = tail
	r.logBase = index

	r.snapshot = append([]byte(nil), data...)
	r.snapshotIndex = index
	r.snapshotTerm = snapTerm

	// Durability invariant: persist before reporting success.
	if err := r.persist(); err != nil {
		return fmt.Errorf("persist raft state after snapshot: %w", err)
	}
	if err := r.persister.SaveSnapshot(r.snapshot, r.snapshotIndex, r.snapshotTerm); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}

	r.logger.Info("snapshot taken",
		slog.Int("index", index),
		slog.Int("term", snapTerm),
		slog.Int("remaining_log", len(r.log)))
	return nil
}
