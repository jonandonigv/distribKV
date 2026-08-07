// Replication subsystem: AppendEntries handler, sendAppendEntries
// (heartbeat-only for step 3c; full log replication in step 3d),
// and the InstallSnapshot stub.
//
// For 3c, the AppendEntries handler processes heartbeats (empty entries):
//   - Rejects stale terms.
//   - Adopts higher terms and steps down.
//   - Records leaderId (only when follower — closes 0.0.x TODO #7).
//   - Resets the election timer.
//
// Full log replication (prevLogIndex/prevLogTerm check, log conflict
// truncation, entry append, leaderCommit advance) is implemented in
// step 3d when client commands start flowing through the log.

package raft

import (
	"context"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AppendEntries handles incoming append requests from a leader.
// Used for both heartbeats (empty entries) and log replication.
//
// For step 3c, only the heartbeat path is exercised. Step 3d adds
// the full prevLogIndex/prevLogTerm consistency check, log conflict
// truncation, entry append, and commitIndex advance via leaderCommit.
func (r *Raft) AppendEntries(ctx context.Context, req *raftpb.AppendEntriesRequest) (*raftpb.AppendEntriesResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := &raftpb.AppendEntriesResponse{
		Term:    int64(r.currentTerm),
		Success: false,
	}

	// Reject stale leader.
	if req.Term < int64(r.currentTerm) {
		return resp, nil
	}

	// Adopt higher term and step down.
	if req.Term > int64(r.currentTerm) {
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		resp.Term = int64(r.currentTerm)
	}

	// Record leader — but only if we're a follower (closes 0.0.x TODO #7).
	// If we're the leader ourselves, a stale leader's AppendEntries should
	// have been rejected by the term check above; if we somehow reach here
	// as leader with the same term, don't overwrite our own leaderId.
	if r.state != Leader {
		r.leaderId = int(req.LeaderId)
	}

	// Reset election timer — we just heard from the leader.
	r.resetElectionTimerLocked()

	// For 3c: heartbeat-only. The full log replication logic (prevLogIndex
	// check, append entries, advance commitIndex via leaderCommit) is
	// implemented in step 3d.
	resp.Success = true
	return resp, nil
}

// sendAppendEntries sends an AppendEntries RPC to one peer. For step 3c
// this is heartbeat-only (empty entries, no log data). Step 3d extends
// it to include log entries starting at peer.nextIndex and to update
// matchIndex/nextIndex on success.
func (r *Raft) sendAppendEntries(peerId int, term int, commitIdx int) {
	r.mu.Lock()
	peer, ok := r.peers[peerId]
	r.mu.Unlock()
	if !ok {
		return
	}

	// Dial lazily.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := peer.ensureConnected(ctx); err != nil {
		return
	}

	r.mu.Lock()
	if r.state != Leader || r.currentTerm != term {
		r.mu.Unlock()
		return
	}

	// Build heartbeat request (no entries for 3c).
	req := &raftpb.AppendEntriesRequest{
		Term:         int64(term),
		LeaderId:     int32(r.serverId),
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries:      nil, // heartbeat
		LeaderCommit: int64(commitIdx),
	}
	r.mu.Unlock()

	resp, err := peer.raftClient.AppendEntries(ctx, req)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Ignore if we're no longer leader or term changed.
	if r.state != Leader || r.currentTerm != term {
		return
	}

	// Step down if peer has a higher term.
	if resp.Term > int64(r.currentTerm) {
		r.stepDown(int(resp.Term))
		return
	}

	// For 3c: heartbeats only. matchIndex/nextIndex updates and
	// updateCommitIndex are implemented in step 3d.
}

// InstallSnapshot is declared in raft.proto for schema stability but
// is not implemented in 0.1.0. The handler returns codes.Unimplemented.
// When snapshotting is implemented, this handler will install the
// snapshot and send it through applyCh (see AGENTS.md "Snapshotting").
func (r *Raft) InstallSnapshot(ctx context.Context, req *raftpb.InstallSnapshotRequest) (*raftpb.InstallSnapshotResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "snapshotting not implemented in 0.1.0")
}
