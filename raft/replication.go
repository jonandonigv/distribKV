// Replication subsystem: AppendEntries handler (full log replication),
// sendAppendEntries (with entries from peer.nextIndex), updateCommitIndex
// (majority + Figure 8 current-term rule), and the public
// ReplicateCommand entry point used by the KV state machine.
//
// See AGENTS.md "Raft Implementation Notes" for the goroutine model and
// concurrency rules.

package raft

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Replication errors (public surface for the KV layer).
var (
	ErrNotLeader = errors.New("not leader")
	ErrTimeout   = errors.New("timeout waiting for commit")
)

// ---------------------------------------------------------------------------
// AppendEntries handler (follower-side).
// ---------------------------------------------------------------------------

// AppendEntries handles incoming append requests from a leader. Implements
// Raft paper Section 5.3: prevLogIndex/term consistency check, log
// conflict truncation, entry append, and commitIndex advance via
// leaderCommit (capped at lastNewIndex).
func (r *Raft) AppendEntries(ctx context.Context, req *raftpb.AppendEntriesRequest) (*raftpb.AppendEntriesResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := &raftpb.AppendEntriesResponse{
		Term:    int64(r.currentTerm),
		Success: false,
	}

	// Rule 1: reject stale leader.
	if req.Term < int64(r.currentTerm) {
		return resp, nil
	}

	// Rule 2: adopt higher term, become follower.
	if req.Term > int64(r.currentTerm) {
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		resp.Term = int64(r.currentTerm)
	}

	// Record leader — only when not leader (closes 0.0.x TODO #7).
	if r.state != Leader {
		r.leaderId = int(req.LeaderId)
	}

	// Reset election timer — we just heard from the leader.
	r.resetElectionTimerLocked()

	// Rule 3: prevLogIndex/prevLogTerm consistency check.
	// Log entries are 0-indexed with logBase; absolute index = logBase + arrayIdx + 1.
	// Access: log[absIndex - logBase - 1].
	if req.PrevLogIndex > 0 {
		arrayIdx := int(req.PrevLogIndex) - r.logBase - 1
		if arrayIdx < 0 || arrayIdx >= len(r.log) {
			// We don't have the entry at prevLogIndex (too far behind
			// or gap). Reject so leader decrements nextIndex and retries.
			return resp, nil
		}
		if r.log[arrayIdx].Term != int64(req.PrevLogTerm) {
			// Term mismatch — reject so leader decrements nextIndex.
			return resp, nil
		}
	}

	// Rule 4: append new entries, truncating any conflicting suffix.
	modified := false
	for i, entry := range req.Entries {
		logIdx := int(req.PrevLogIndex) + i + 1 // absolute Raft index of this entry
		arrayIdx := logIdx - r.logBase - 1

		if arrayIdx < len(r.log) {
			// We already have an entry at this index. Check for conflict.
			if r.log[arrayIdx].Term != int64(entry.Term) {
				// Conflict: truncate from here and append the new entry.
				r.log = r.log[:arrayIdx]
				r.log = append(r.log, LogEntry{
					Index:   entry.Index,
					Term:    int64(entry.Term),
					Command: append([]byte(nil), entry.Command...),
				})
				modified = true
			}
			// Same entry already present — skip (idempotent).
		} else {
			// New entry beyond our current log.
			r.log = append(r.log, LogEntry{
				Index:   entry.Index,
				Term:    int64(entry.Term),
				Command: append([]byte(nil), entry.Command...),
			})
			modified = true
		}
	}

	// Rule 5: persist if we modified the log.
	if modified {
		if err := r.persist(); err != nil {
			r.logger.Error("persist failed on AppendEntries", "err", err)
			return nil, fmt.Errorf("persist on append: %w", err)
		}
	}

	// Rule 6: advance commitIndex via leaderCommit (capped at last new entry).
	lastNewIndex := int(req.PrevLogIndex) + len(req.Entries)
	if req.LeaderCommit > int64(r.commitIndex) {
		newCommit := int(req.LeaderCommit)
		if newCommit > lastNewIndex {
			newCommit = lastNewIndex
		}
		if newCommit > r.commitIndex {
			r.commitIndex = newCommit
			r.signalCommit()
		}
	}

	resp.Success = true
	return resp, nil
}

// ---------------------------------------------------------------------------
// sendAppendEntries (leader-side).
// ---------------------------------------------------------------------------

// sendAppendEntries sends an AppendEntries RPC to one peer, including log
// entries starting at peer.nextIndex. On success, updates matchIndex and
// nextIndex and calls updateCommitIndex. On log mismatch (success=false),
// decrements nextIndex and retries. On higher term, steps down.
//
// Goroutine spawned by runHeartbeat and by ReplicateCommand.
func (r *Raft) sendAppendEntries(peerId int, term int, commitIdx int) {
	if r.shutdown.Load() {
		return
	}

	r.mu.Lock()
	peer, ok := r.peers[peerId]
	r.mu.Unlock()
	if !ok {
		return
	}

	// Dial lazily (see AGENTS.md gRPC Guidelines).
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer dialCancel()
	if err := peer.ensureConnected(dialCtx); err != nil {
		return
	}

	r.mu.Lock()
	if r.state != Leader || r.currentTerm != term || r.shutdown.Load() {
		r.mu.Unlock()
		return
	}

	// Build the request with entries starting at peer.nextIndex.
	nextIdx := peer.nextIndex
	if nextIdx < 1 {
		nextIdx = 1
	}
	prevLogIdx := nextIdx - 1
	prevLogTerm := int64(0)
	if prevLogIdx > 0 {
		arrayIdx := prevLogIdx - r.logBase - 1
		if arrayIdx >= 0 && arrayIdx < len(r.log) {
			prevLogTerm = r.log[arrayIdx].Term
		} else {
			prevLogIdx = 0
			prevLogTerm = 0
			nextIdx = 1
		}
	}

	// Collect entries from nextIdx to end of log.
	var entries []*raftpb.LogEntry
	for i := nextIdx; i <= len(r.log); i++ {
		arrayIdx := i - r.logBase - 1
		if arrayIdx < 0 || arrayIdx >= len(r.log) {
			break
		}
		e := &r.log[arrayIdx]
		entries = append(entries, &raftpb.LogEntry{
			Index:   e.Index,
			Term:    e.Term,
			Command: append([]byte(nil), e.Command...),
		})
	}

	req := &raftpb.AppendEntriesRequest{
		Term:         int64(term),
		LeaderId:     int32(r.serverId),
		PrevLogIndex: int64(prevLogIdx),
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: int64(commitIdx),
	}
	r.mu.Unlock()

	// Make the RPC call (outside the lock).
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer rpcCancel()

	// Snapshot the raftClient under connMu to safely race with Shutdown.
	peer.connMu.RLock()
	raftClient := peer.raftClient
	peer.connMu.RUnlock()
	if raftClient == nil {
		return
	}
	resp, err := raftClient.AppendEntries(rpcCtx, req)
	if err != nil {
		return
	}

	r.mu.Lock()

	// Ignore if we're no longer leader or term changed or shutting down.
	if r.state != Leader || r.currentTerm != term || r.shutdown.Load() {
		r.mu.Unlock()
		return
	}

	// Step down if peer has a higher term.
	if resp.Term > int64(r.currentTerm) {
		r.stepDown(int(resp.Term))
		r.mu.Unlock()
		return
	}

	if resp.Success {
		// Update matchIndex and nextIndex.
		newMatch := prevLogIdx + len(entries)
		if newMatch > peer.matchIndex {
			peer.matchIndex = newMatch
		}
		peer.nextIndex = peer.matchIndex + 1
		r.updateCommitIndex()
		r.mu.Unlock()
	} else {
		// Log mismatch — decrement nextIndex and retry.
		if peer.nextIndex > 1 {
			peer.nextIndex--
		}
		r.mu.Unlock()
		// Spawn retry AFTER releasing the lock (AGENTS.md concurrency:
		// never call RPCs while holding locks; never spawn goroutines
		// while holding locks).
		go r.sendAppendEntries(peerId, term, commitIdx)
	}
}

// updateCommitIndex scans for the highest N where a majority of peers
// have matchIndex >= N, and advances commitIndex if log[N].Term ==
// currentTerm (Figure 8 safety). All entries below N commit transitively.
// Caller must hold r.mu.
func (r *Raft) updateCommitIndex() {
	if r.state != Leader {
		return
	}

	// Scan from the highest log index downward to find the highest N
	// with majority replication. Figure 8: only commit if that entry
	// is from the current term. If it isn't, we don't commit at all
	// (a previous-term entry alone can't commit without a current-term
	// entry also being committed).
	lastIdx := len(r.log) + r.logBase
	for n := lastIdx; n > r.commitIndex; n-- {
		arrayIdx := n - r.logBase - 1
		if arrayIdx < 0 || arrayIdx >= len(r.log) {
			continue
		}

		count := 1 // self always has the entry.
		for _, peer := range r.peers {
			if peer.matchIndex >= n {
				count++
			}
		}

		if count > (len(r.peers)+1)/2 {
			// Majority has replicated entry N.
			if r.log[arrayIdx].Term == int64(r.currentTerm) {
				r.commitIndex = n
				r.signalCommit()
			}
			// Either committed or can't (wrong term). Either way,
			// this is the highest majority entry, so we're done.
			break
		}
	}
}

// signalCommit sends a non-blocking wake to commitCh so the apply loop
// re-checks commitIndex. Dropped signals are harmless because applyLoop
// re-reads commitIndex under the mutex. Caller must hold r.mu.
func (r *Raft) signalCommit() {
	select {
	case r.commitCh <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// ReplicateCommand (public, called by the KV state machine).
// ---------------------------------------------------------------------------

// ReplicateCommand submits a command to the Raft log. Only the leader
// accepts commands. Non-leaders return ErrNotLeader. The command is
// appended, persisted, and replicated to peers. The call blocks until
// the command is committed (majority replication) or a 5s timeout.
//
// Returns (logIndex, nil) on success, (logIndex, ErrTimeout) on timeout
// (the command may still commit later), or (0, ErrNotLeader) if this
// node is not the leader.
func (r *Raft) ReplicateCommand(cmd []byte) (int, error) {
	r.mu.Lock()
	if r.state != Leader {
		r.mu.Unlock()
		return 0, ErrNotLeader
	}

	// Append to local log.
	nextIdx := len(r.log) + r.logBase + 1
	entry := LogEntry{
		Index:   int64(nextIdx),
		Term:    int64(r.currentTerm),
		Command: append([]byte(nil), cmd...),
	}
	r.log = append(r.log, entry)

	// Persist immediately (durability invariant).
	if err := r.persist(); err != nil {
		// Roll back the append — we can't respond with a durable entry.
		r.log = r.log[:len(r.log)-1]
		r.mu.Unlock()
		return 0, fmt.Errorf("persist command: %w", err)
	}

	// Try to commit immediately (single-node self-commits here; multi-node
	// also gets a head start before the replication goroutines report back).
	r.updateCommitIndex()

	// Collect peer IDs under lock, then spawn sendAppendEntries after
	// releasing (AGENTS.md concurrency: never call RPCs while holding locks).
	peerIds := make([]int, 0, len(r.peers))
	for id := range r.peers {
		peerIds = append(peerIds, id)
	}
	term := r.currentTerm
	commitIdx := r.commitIndex
	targetIdx := nextIdx
	r.mu.Unlock()

	// Fan out replication.
	for _, id := range peerIds {
		go r.sendAppendEntries(id, term, commitIdx)
	}

	// Wait for commit with a 5s timeout. We poll commitIndex every 10ms
	// (production code; tests use require.Eventually for their async).
	// This is the 0.0.x pattern — simple and correct.
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout.C:
			return targetIdx, ErrTimeout
		case <-ticker.C:
			r.mu.Lock()
			committed := r.commitIndex >= targetIdx
			shutdown := r.shutdown.Load()
			r.mu.Unlock()
			if shutdown {
				return targetIdx, ErrTimeout
			}
			if committed {
				return targetIdx, nil
			}
		case <-r.ctx.Done():
			return targetIdx, ErrTimeout
		}
	}
}

// ---------------------------------------------------------------------------
// InstallSnapshot (deferred — seam baked in per AGENTS.md).
// ---------------------------------------------------------------------------

// InstallSnapshot is declared in raft.proto for schema stability but is
// not implemented in 0.1.0. Returns codes.Unimplemented.
func (r *Raft) InstallSnapshot(ctx context.Context, req *raftpb.InstallSnapshotRequest) (*raftpb.InstallSnapshotResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "snapshotting not implemented in 0.1.0")
}
