package raft

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	pb "github.com/jonandonigv/distribKV/proto/raft"
)

// runElectionTimer is the background goroutine that manages election timeouts.
// It uses a select statement to handle timer expiration, reset signals, and stop signals.
// When the timer expires, it triggers a new election by calling becomeCandidate().
// resetCh and stopCh are captured at Start() time so reads in the select are
// race-free against Shutdown closing/resetting the channel fields on r.
func (r *Raft) runElectionTimer(resetCh, stopCh chan struct{}) {
	for {
		// Calculate timeout duration
		var timeout time.Duration
		r.mu.Lock()
		if r.useDeterministicTimeout {
			timeout = r.deterministicTimeout
		} else {
			timeout = r.electionTimeoutMin + time.Duration(rand.Intn(int(r.electionTimeoutMax-r.electionTimeoutMin)))
		}
		doneChan := r.electionDoneChan
		r.mu.Unlock()

		timer := time.NewTimer(timeout)

		// Build select cases dynamically
		if doneChan != nil {
			// Election in progress - wait for completion or timeout
			select {
			case <-timer.C:
				// Timer expired - start new election
				log.Printf("[Raft %d] Election timeout fired (no heartbeat), starting election", r.serverId)
				r.becomeCandidate()

			case <-doneChan:
				// All RPCs completed - start new election immediately
				timer.Stop()
				r.mu.Lock()
				r.electionDoneChan = nil
				r.consecutiveFailedElections++
				r.mu.Unlock()
				log.Printf("[Raft %d] Election completed without majority, starting new election", r.serverId)
				r.becomeCandidate()

			case <-resetCh:
				// Reset requested - drain any additional reset signals and continue
				timer.Stop()
				select {
				case <-resetCh:
					// Drain additional resets
				default:
				}
				continue

			case <-stopCh:
				// Stop requested - exit goroutine
				timer.Stop()
				return
			}
		} else {
			// No election in progress - normal timeout behavior
			select {
			case <-timer.C:
				// Timer expired - start new election
				log.Printf("[Raft %d] Election timeout fired (no heartbeat), starting election", r.serverId)
				r.becomeCandidate()

			case <-resetCh:
				// Reset requested - drain any additional reset signals and continue
				timer.Stop()
				select {
				case <-resetCh:
					// Drain additional resets
				default:
				}
				continue

			case <-stopCh:
				// Stop requested - exit goroutine
				timer.Stop()
				return
			}
		}
	}
}

// becomeCandidate transitions the node to candidate state and starts a new election.
// It increments the term, votes for itself, resets the election timer, and sends
// RequestVote RPCs to all peers concurrently.
func (r *Raft) becomeCandidate() {
	// Apply backoff if we've had consecutive failed elections
	r.mu.Lock()
	failedCount := r.consecutiveFailedElections
	r.mu.Unlock()

	if failedCount > 0 {
		backoff := time.Duration(rand.Intn(300))*time.Millisecond +
			time.Duration(failedCount*150)*time.Millisecond
		if backoff > 3*time.Second {
			backoff = 3 * time.Second
		}
		r.debugLog("[Raft %d] Backing off for %v after %d failed elections", r.serverId, backoff, failedCount)
		time.Sleep(backoff)
	}

	r.mu.Lock()

	// Don't start a new election if we are shutting down.
	if r.shutdown {
		r.mu.Unlock()
		return
	}

	// Only become candidate if we're a follower or candidate
	// (we might have already become leader or stepped down)
	if r.state == Leader {
		r.mu.Unlock()
		return
	}

	oldState := r.state
	// Increment term and vote for self
	r.currentTerm++
	r.votedFor = r.serverId
	r.state = Candidate
	r.leaderId = -1 // Starting election - clear leader knowledge

	log.Printf("[Raft %d] State change: %v -> Candidate (term %d)", r.serverId, oldState, r.currentTerm)

	// Reset vote count (self-vote = 1)
	r.votesMutex.Lock()
	r.votesReceived = 1
	r.votesMutex.Unlock()

	// Single-node cluster: the self-vote is a majority. Promote directly to
	// Leader without dispatching any RPCs. (becomeLeader acquires r.mu itself,
	// so release it first.)
	if len(r.peers) == 0 {
		term := r.currentTerm
		r.mu.Unlock()
		r.debugLog("[Raft %d] Single-node cluster: self-electing leader for term %d", r.serverId, term)
		r.becomeLeader()
		return
	}

	// Initialize election tracking
	r.pendingVoteRpcs = int32(len(r.peers))
	r.electionDoneChan = make(chan struct{})

	// Get current term for RPC calls
	term := r.currentTerm
	r.mu.Unlock()

	// Reset election timer for this term (self-vote resets timer)
	r.resetElectionTimer()

	r.debugLog("[Raft %d] Sending RequestVote to %d peers (term %d)", r.serverId, len(r.peers), term)

	// Send RequestVote to all peers concurrently
	for peerId := range r.peers {
		go r.sendRequestVote(peerId, term)
	}
}

// sendRequestVote sends a RequestVote RPC to a specific peer.
// It handles RPC failures, vote grants, and higher term detection.
func (r *Raft) sendRequestVote(peerId int, term int) {
	// Track RPC completion
	defer func() {
		if atomic.AddInt32(&r.pendingVoteRpcs, -1) == 0 {
			r.mu.Lock()
			if r.electionDoneChan != nil {
				close(r.electionDoneChan)
			}
			r.mu.Unlock()
		}
	}()

	// Get peer
	peer, ok := r.peers[peerId]
	if !ok {
		return
	}

	// Ensure connection is healthy
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	if err := peer.ensureConnected(connectCtx); err != nil {
		connectCancel()
		log.Printf("[Raft %d] Cannot reach peer %d: %v", r.serverId, peerId, err)
		return
	}
	connectCancel()

	// Get last log info
	r.mu.Lock()
	lastLogIndex, lastLogTerm := r.getLastLogInfo()
	r.mu.Unlock()

	// Build request
	args := &pb.RequestVoteRequest{
		Term:         int64(term),
		CandidateId:  int32(r.serverId),
		LastLogIndex: int64(lastLogIndex),
		LastLogTerm:  int64(lastLogTerm),
	}

	// Make RPC call with timeout (fast operation, no log transfer)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	reply, err := peer.raftClient.RequestVote(ctx, args)
	if err != nil {
		// RPC failed - don't count this as a rejection
		log.Printf("[Raft %d] RequestVote RPC to peer %d failed: %v", r.serverId, peerId, err)
		return
	}

	// Handle response
	r.mu.Lock()

	// Check if term changed while we were waiting (election already finished)
	if r.currentTerm != term {
		log.Printf("[Raft %d] Election already finished (term changed), ignoring vote from %d", r.serverId, peerId)
		r.mu.Unlock()
		return
	}

	// Check for higher term
	if reply.Term > int64(r.currentTerm) {
		log.Printf("[Raft %d] Received higher term %d from peer %d, stepping down", r.serverId, reply.Term, peerId)
		r.stepDown(int(reply.Term))
		r.mu.Unlock()
		return
	}

	// Check if vote was granted
	if reply.VoteGranted {
		r.votesMutex.Lock()
		r.votesReceived++
		votes := r.votesReceived
		r.votesMutex.Unlock()

		r.debugLog("[Raft %d] Received vote from peer %d (total: %d/%d)", r.serverId, peerId, votes, len(r.peers)/2+1)

		// Check if we have majority and are still a candidate
		// Must hold r.mu when reading r.state to avoid race condition

		isCandidate := r.state == Candidate

		if votes > len(r.peers)/2 && isCandidate {
			r.mu.Unlock()
			r.becomeLeader()
			return
		}
	} else {
		r.debugLog("[Raft %d] Peer %d rejected vote request", r.serverId, peerId)
	}
	r.mu.Unlock()
}

// stepDown transitions the node to follower state with the given term.
// It updates the term, resets votedFor, and resets the election timer.
// Must be called while holding r.mu.
func (r *Raft) stepDown(newTerm int) {
	oldState := r.state
	r.currentTerm = newTerm
	r.votedFor = -1
	r.state = Follower
	r.leaderId = -1 // Clear leader knowledge when stepping down

	// Clear election tracking
	if r.electionDoneChan != nil {
		select {
		case <-r.electionDoneChan:
			// Already closed
		default:
			close(r.electionDoneChan)
		}
		r.electionDoneChan = nil
	}

	// Reset failed election counter - we found a leader
	r.consecutiveFailedElections = 0

	log.Printf("[Raft %d] State change: %v -> Follower (term %d)", r.serverId, oldState, newTerm)
	r.resetElectionTimer()
}

// resetElectionTimer sends a signal to reset the election timer.
// This is non-blocking and handles the case where the channel is full.
func (r *Raft) resetElectionTimer() {
	select {
	case r.electionResetChan <- struct{}{}:
		// Signal sent successfully
	default:
		// Channel full, signal will be processed on next iteration
	}
}

// stopElectionTimer sends a signal to stop the election timer goroutine.
// Idempotent. Caller MUST hold r.mu. Shutdown closes the channel inline
// rather than calling this helper (since the election timer is started
// outside the lock and reads a captured local).
func (r *Raft) stopElectionTimer() {
	if r.electionStopChan != nil {
		close(r.electionStopChan)
		r.electionStopChan = nil
	}
}

// becomeLeader transitions the node to leader state.
// Called when candidate receives majority of votes.
func (r *Raft) becomeLeader() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check we're still a candidate
	if r.state != Candidate {
		return
	}

	log.Printf("[Raft %d] *** ELECTED LEADER (term %d) ***", r.serverId, r.currentTerm)
	r.state = Leader
	r.leaderId = r.serverId // Self-aware: track self as leader

	// Reset failed election counter - we won!
	r.consecutiveFailedElections = 0

	// Clear election tracking (we won, no need for electionDoneChan)
	r.electionDoneChan = nil

	// Stop election timer - leader doesn't need it
	r.stopElectionTimer()

	// Initialize leader state for each peer
	for _, peer := range r.peers {
		peer.nextIndex = len(r.log) + 1
		peer.matchIndex = 0
	}

	// Start heartbeat sender goroutine
	r.startHeartbeat()
}

// RequestVote handles incoming vote requests from candidates.
// Called when another server requests our vote.
func (r *Raft) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reply := &pb.RequestVoteResponse{
		Term:        int64(r.currentTerm),
		VoteGranted: false,
	}

	if req.Term < int64(r.currentTerm) {
		return reply, nil
	}

	if req.Term > int64(r.currentTerm) {
		log.Printf("[Raft %d] Received RequestVote from candidate %d with higher term %d, updating term", r.serverId, req.CandidateId, req.Term)
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		reply.Term = req.Term
		r.resetElectionTimer()
	}

	if r.votedFor == -1 || r.votedFor == int(req.CandidateId) {
		lastLogIndex := len(r.log)
		lastLogTerm := 0
		if lastLogIndex > 0 {
			lastLogTerm = r.log[lastLogIndex-1].Term
		}

		if req.LastLogTerm > int64(lastLogTerm) ||
			(req.LastLogTerm == int64(lastLogTerm) && req.LastLogIndex >= int64(lastLogIndex)) {

			newVotedFor := int(req.CandidateId)
			newTerm := r.currentTerm
			logCopy := r.log

			r.mu.Unlock()
			err := r.persister.Save(newTerm, newVotedFor, logCopy)
			r.mu.Lock()

			if err != nil {
				log.Printf("[Raft %d] CRITICAL: Failed to persist vote for candidate %d: %v", r.serverId, req.CandidateId, err)
				return nil, fmt.Errorf("failed to persist vote: %w", err)
			}

			r.votedFor = newVotedFor
			r.currentTerm = newTerm
			reply.VoteGranted = true
			log.Printf("[Raft %d] Granted vote to candidate %d for term %d", r.serverId, req.CandidateId, req.Term)
		} else {
			log.Printf("[Raft %d] Denied vote to candidate %d: log not up-to-date (candidate: %d@%d, me: %d@%d)",
				r.serverId, req.CandidateId, req.LastLogIndex, req.LastLogTerm, lastLogIndex, lastLogTerm)
		}
	} else {
		log.Printf("[Raft %d] Denied vote to candidate %d: already voted for %d in term %d",
			r.serverId, req.CandidateId, r.votedFor, r.currentTerm)
	}

	return reply, nil
}

// startHeartbeat initializes and starts the heartbeat sender goroutine.
// Must be called when node becomes leader. Caller MUST hold r.mu.
func (r *Raft) startHeartbeat() {
	stopCh := make(chan struct{})
	r.heartbeatStopChan = stopCh
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runHeartbeat(stopCh)
	}()
}

// stopHeartbeat signals the heartbeat sender goroutine to stop.
// Must be called when leader steps down. Caller MUST hold r.mu. Idempotent.
func (r *Raft) stopHeartbeat() {
	if r.heartbeatStopChan != nil {
		close(r.heartbeatStopChan)
		r.heartbeatStopChan = nil
	}
}

// runHeartbeat is the background goroutine that sends periodic heartbeats to all peers.
// It runs until stopCh is closed or it notices the node is no longer leader.
func (r *Raft) runHeartbeat(stopCh chan struct{}) {
	log.Printf("[Raft %d] Heartbeat sender started (interval: %v)", r.serverId, r.heartbeatInterval)
	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Send heartbeat to all peers
			r.mu.Lock()
			if r.state != Leader || r.shutdown {
				log.Printf("[Raft %d] Heartbeat sender stopping: no longer leader", r.serverId)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()

			for peerId := range r.peers {
				go r.sendAppendEntries(peerId)
			}

		case <-stopCh:
			log.Printf("[Raft %d] Heartbeat sender stopped", r.serverId)
			return
		}
	}
}
