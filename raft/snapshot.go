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
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
)

// maxSnapshotBytes caps a snapshot both locally (Raft.Snapshot refuses to
// store bigger) and on the wire (InstallSnapshot handler rejects them).
// 4MB per PLAN.md / AGENTS.md; single-chunk transfer in 0.2.0.
const maxSnapshotBytes = 4 * 1024 * 1024

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

	if len(data) > maxSnapshotBytes {
		return fmt.Errorf("snapshot %d bytes exceeds cap %d", len(data), maxSnapshotBytes)
	}
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

// sendInstallSnapshot ships the leader's current snapshot to one peer.
// Spawned by sendAppendEntries when the peer's nextIndex predates
// logBase (AppendEntries can never describe that territory). On success
// the peer is considered caught up through snapshotIndex, and ordinary
// AppendEntries resumes from there next heartbeat. Runs in its own
// goroutine; never called with r.mu held (AGENTS.md concurrency rules).
func (r *Raft) sendInstallSnapshot(peerId int, term int) {
	if r.shutdown.Load() {
		return
	}

	r.mu.Lock()
	peer, ok := r.peers[peerId]
	if !ok {
		r.mu.Unlock()
		return
	}
	if len(r.snapshot) == 0 {
		r.mu.Unlock()
		return // nothing to send — shouldn't happen when logBase > 0
	}
	data := append([]byte(nil), r.snapshot...)
	lastIdx, lastTerm := r.snapshotIndex, r.snapshotTerm
	r.mu.Unlock()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer dialCancel()
	if err := peer.ensureConnected(dialCtx); err != nil {
		return
	}

	peer.connMu.RLock()
	client := peer.raftClient
	peer.connMu.RUnlock()
	if client == nil {
		return
	}

	req := &raftpb.InstallSnapshotRequest{
		Term:              int64(term),
		LeaderId:          int32(r.serverId),
		LastIncludedIndex: int64(lastIdx),
		LastIncludedTerm:  int64(lastTerm),
		Offset:            0,
		Data:              data,
		Done:              true,
	}
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rpcCancel()

	resp, err := client.InstallSnapshot(rpcCtx, req)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != Leader || r.currentTerm != term || r.shutdown.Load() {
		return
	}
	if resp.Term > int64(r.currentTerm) {
		r.logger.Info("stepping down: InstallSnapshot peer has higher term",
			slog.Int("peer", peerId), slog.Int("peer_term", int(resp.Term)))
		r.stepDown(int(resp.Term))
		return
	}
	// Success (or benign duplicate): the peer now holds everything
	// through the snapshot point.
	if lastIdx > peer.matchIndex {
		peer.matchIndex = lastIdx
	}
	peer.nextIndex = peer.matchIndex + 1
	r.updateCommitIndex()
}
