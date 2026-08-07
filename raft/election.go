// Election subsystem: RequestVote handler, becomeCandidate, becomeLeader,
// stepDown, heartbeat sender. See AGENTS.md "Goroutine model".

package raft

import (
	"context"
	"fmt"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
)

// becomeCandidate transitions the node to Candidate state, increments the
// term, votes for self, persists, and sends RequestVote to all peers.
// Called by the election timer (AfterFunc) and by tests. No-op if the
// node is already Leader or is shutting down.
func (r *Raft) becomeCandidate() {
	r.mu.Lock()
	if r.shutdown.Load() {
		r.mu.Unlock()
		return
	}
	if r.state == Leader {
		r.mu.Unlock()
		return
	}

	r.state = Candidate
	r.currentTerm++
	r.votedFor = r.serverId
	r.leaderId = -1
	r.votesReceived = 1 // self-vote

	// Persist before doing anything else (Raft persist-before-respond).
	if err := r.persist(); err != nil {
		r.logger.Error("persist failed on becomeCandidate",
			"err", err,
			"term", r.currentTerm)
		r.mu.Unlock()
		return
	}

	term := r.currentTerm
	peers := make([]int, 0, len(r.peers))
	for id := range r.peers {
		peers = append(peers, id)
	}
	r.mu.Unlock()

	r.logger.Info("became candidate", "term", term)

	// Re-arm the election timer. If this election fails (no majority),
	// the timer will fire and start a new one. If we win, becomeLeader
	// will stop the timer.
	r.mu.Lock()
	r.resetElectionTimerLocked()
	r.mu.Unlock()

	// Single node: self-elect immediately.
	if len(peers) == 0 {
		r.becomeLeader()
		return
	}

	// Send RequestVote to all peers concurrently. Goroutines are spawned
	// AFTER releasing the lock (see AGENTS.md concurrency patterns).
	for _, peerId := range peers {
		go r.sendRequestVote(peerId, term)
	}
}

// sendRequestVote sends a RequestVote RPC to one peer and handles the
// response. Runs in its own goroutine (spawned by becomeCandidate or
// by the election retry path). On majority, calls becomeLeader.
func (r *Raft) sendRequestVote(peerId int, term int) {
	r.mu.Lock()
	peer, ok := r.peers[peerId]
	r.mu.Unlock()
	if !ok {
		return
	}

	// Dial the peer lazily (see AGENTS.md gRPC Guidelines).
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := peer.ensureConnected(ctx); err != nil {
		r.logger.Debug("cannot reach peer for RequestVote",
			"peer", peerId, "err", err)
		return
	}

	// Read last-log info under the lock.
	r.mu.Lock()
	lastIdx, lastTerm := r.getLastLogInfoLocked()
	r.mu.Unlock()

	resp, err := peer.raftClient.RequestVote(ctx, &raftpb.RequestVoteRequest{
		Term:         int64(term),
		CandidateId:  int32(r.serverId),
		LastLogIndex: int64(lastIdx),
		LastLogTerm:  int64(lastTerm),
	})
	if err != nil {
		r.logger.Debug("RequestVote RPC failed",
			"peer", peerId, "err", err)
		return
	}

	r.mu.Lock()

	// Ignore if election already finished (term advanced or stepped down).
	if r.currentTerm != term || r.state != Candidate {
		r.mu.Unlock()
		return
	}

	// Step down if peer has a higher term.
	if resp.Term > int64(r.currentTerm) {
		r.logger.Info("stepping down: peer has higher term",
			"peer", peerId, "peer_term", resp.Term, "my_term", r.currentTerm)
		r.stepDown(int(resp.Term))
		r.mu.Unlock()
		return
	}

	if resp.VoteGranted {
		r.votesReceived++
		r.logger.Debug("got vote",
			"peer", peerId, "votes", r.votesReceived,
			"need", len(r.peers)/2+1)

		// Majority check: votes > peers/2 (self-vote already counted).
		if r.votesReceived > len(r.peers)/2 {
			r.mu.Unlock()
			r.becomeLeader()
			return
		}
	}
	r.mu.Unlock()
}

// RequestVote handles incoming vote requests from candidates.
// Implements the RequestVote RPC from the Raft paper (Section 5.2).
func (r *Raft) RequestVote(ctx context.Context, req *raftpb.RequestVoteRequest) (*raftpb.RequestVoteResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	resp := &raftpb.RequestVoteResponse{
		Term:        int64(r.currentTerm),
		VoteGranted: false,
	}

	// Reject if candidate's term is stale.
	if req.Term < int64(r.currentTerm) {
		return resp, nil
	}

	// Adopt higher term, become follower, reset vote.
	if req.Term > int64(r.currentTerm) {
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		resp.Term = req.Term
	}

	// Grant vote only if we haven't voted (or already voted for this
	// candidate) AND the candidate's log is at least as up-to-date.
	canVote := r.votedFor == -1 || r.votedFor == int(req.CandidateId)
	if canVote {
		lastIdx, lastTerm := r.getLastLogInfoLocked()
		logOk := int64(req.LastLogTerm) > int64(lastTerm) ||
			(int64(req.LastLogTerm) == int64(lastTerm) && int64(req.LastLogIndex) >= int64(lastIdx))
		if logOk {
			// Persist BEFORE granting the vote (persist-before-respond).
			if err := r.persist(); err != nil {
				r.logger.Error("persist failed on vote grant",
					"err", err, "candidate", req.CandidateId)
				return nil, fmt.Errorf("persist vote: %w", err)
			}
			r.votedFor = int(req.CandidateId)
			resp.VoteGranted = true
			resp.Term = int64(r.currentTerm)
			r.resetElectionTimerLocked()
			r.logger.Info("granted vote",
				"candidate", req.CandidateId, "term", r.currentTerm)
		}
	}

	return resp, nil
}

// becomeLeader transitions the node to Leader, initialises per-peer
// replication state, stops the election timer, and starts the heartbeat
// sender. Called when a candidate wins a majority. No-op if no longer
// a candidate (e.g. stepped down concurrently).
func (r *Raft) becomeLeader() {
	r.mu.Lock()
	if r.state != Candidate || r.shutdown.Load() {
		r.mu.Unlock()
		return
	}

	r.state = Leader
	r.leaderId = r.serverId

	// Initialise nextIndex/matchIndex for each peer.
	lastIdx := 0
	if len(r.log) > 0 {
		lastIdx = int(r.log[len(r.log)-1].Index)
	}
	for _, p := range r.peers {
		p.nextIndex = lastIdx + 1
		p.matchIndex = 0
	}

	// Stop election timer — leader doesn't need it.
	if r.electionTimer != nil {
		r.electionTimer.Stop()
	}

	term := r.currentTerm
	r.mu.Unlock()

	r.logger.Info("elected leader", "term", term)

	// Start heartbeat sender (spawns a goroutine tracked by WaitGroup).
	r.startHeartbeatLocked()
}

// stepDown transitions the node to Follower with the given term.
// Resets votedFor and leaderId. Caller must hold r.mu.
func (r *Raft) stepDown(newTerm int) {
	oldState := r.state
	r.currentTerm = newTerm
	r.votedFor = -1
	r.state = Follower
	r.leaderId = -1
	r.votesReceived = 0
	r.logger.Info("stepped down",
		"from", oldState.String(), "term", newTerm)
}

// ---------------------------------------------------------------------------
// Heartbeat sender (leader-only goroutine).
// ---------------------------------------------------------------------------

// startHeartbeatLocked starts the heartbeat sender goroutine. The goroutine
// is tracked by the WaitGroup so Shutdown can wait for it. Caller must NOT
// hold r.mu (this method does not acquire it but adds to the WaitGroup;
// calling Add while another goroutine calls Wait is safe but only if
// Shutdown hasn't started yet — guaranteed because the leader is active).
func (r *Raft) startHeartbeatLocked() {
	r.wg.Add(1)
	go r.runHeartbeat()
}

// runHeartbeat periodically sends AppendEntries (heartbeats) to all peers.
// Exits when ctx is cancelled (Shutdown) or when the node is no longer
// leader (checked on each tick).
func (r *Raft) runHeartbeat() {
	defer r.wg.Done()

	// Read ctx safely — may be nil if Start() was never called (tests
	// that invoke becomeLeader directly without the full lifecycle).
	r.mu.Lock()
	ctx := r.ctx
	r.mu.Unlock()
	if ctx == nil {
		return
	}

	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()

	// Send one round immediately so followers don't time out while
	// waiting for the first ticker tick.
	r.sendHeartbeats()

	for {
		select {
		case <-ticker.C:
			r.sendHeartbeats()
		case <-ctx.Done():
			return
		}
	}
}

// sendHeartbeats fans out an AppendEntries (heartbeat) to every peer.
// Goroutines are spawned after releasing the lock (concurrency pattern
// per AGENTS.md).
func (r *Raft) sendHeartbeats() {
	r.mu.Lock()
	if r.state != Leader || r.shutdown.Load() {
		r.mu.Unlock()
		return
	}
	term := r.currentTerm
	commitIdx := r.commitIndex
	peerIds := make([]int, 0, len(r.peers))
	for id := range r.peers {
		peerIds = append(peerIds, id)
	}
	r.mu.Unlock()

	for _, id := range peerIds {
		go r.sendAppendEntries(id, term, commitIdx)
	}
}
