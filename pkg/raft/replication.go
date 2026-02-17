package raft

// Package raft implements the Raft consensus algorithm.
// It provides leader election, log replication, and state machine application.
//
// Replication Flow:
// 1. Client submits command to leader via replicateCommand()
// 2. Leader appends to local log
// 3. Leader sends AppendEntries to all followers via sendAppendEntries()
// 4. Followers acknowledge or reject (log mismatch)
// 5. Leader updates matchIndex/nextIndex on success
// 6. Leader advances commitIndex when majority replicated via updateCommitIndex()
// 7. Committed entries are applied to state machine via applyCommittedEntries()

import (
	"context"
	"fmt"
	"log"
	"time"

	pb "github.com/jonandonigv/distribKV/proto/raft"
)

// AppendEntries handles incoming append requests from leader.
// Used for both heartbeat (empty entries) and log replication.
func (r *Raft) AppendEntries(ctx context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reply := &pb.AppendEntriesResponse{
		Term:    int64(r.currentTerm),
		Success: false,
	}

	if req.Term < int64(r.currentTerm) {
		log.Printf("[Raft %d] Rejected AppendEntries from leader %d: stale term %d < %d", r.serverId, req.LeaderId, req.Term, r.currentTerm)
		return reply, nil
	}

	savedCurrentTerm := r.currentTerm
	savedVotedFor := r.votedFor
	savedLog := make([]LogEntry, len(r.log))
	copy(savedLog, r.log)

	needRollback := false

	if req.Term > int64(r.currentTerm) {
		log.Printf("[Raft %d] Received AppendEntries from leader %d with higher term %d, updating term", r.serverId, req.LeaderId, req.Term)
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		reply.Term = req.Term
		needRollback = true
	}

	if r.leaderId != int(req.LeaderId) {
		log.Printf("[Raft %d] Discovered leader %d for term %d", r.serverId, req.LeaderId, req.Term)
	}
	r.leaderId = int(req.LeaderId)

	r.resetElectionTimer()

	if req.PrevLogIndex > 0 {
		if int(req.PrevLogIndex) > len(r.log) {
			log.Printf("[Raft %d] Log mismatch at index %d: don't have entry", r.serverId, req.PrevLogIndex)
			return reply, nil
		}
		if r.log[req.PrevLogIndex-1].Term != int(req.PrevLogTerm) {
			log.Printf("[Raft %d] Log conflict at index %d: term %d != %d, truncating log", r.serverId, req.PrevLogIndex, r.log[req.PrevLogIndex-1].Term, req.PrevLogTerm)
			r.log = r.log[:req.PrevLogIndex-1]
			return reply, nil
		}
	}

	entriesAppended := 0
	for i, entry := range req.Entries {
		logIndex := int(req.PrevLogIndex) + i
		arrayIndex := logIndex - 1

		if arrayIndex < len(r.log) {
			if r.log[arrayIndex].Term != int(entry.Term) {
				log.Printf("[Raft %d] Conflict at index %d: replacing term %d with %d", r.serverId, logIndex, r.log[arrayIndex].Term, entry.Term)
				r.log = r.log[:arrayIndex]
				r.log = append(r.log, LogEntry{
					Index:   entry.Index,
					Term:    int(entry.Term),
					Command: entry.Command,
				})
				entriesAppended++
				needRollback = true
			}
		} else {
			r.log = append(r.log, LogEntry{
				Index:   entry.Index,
				Term:    int(entry.Term),
				Command: entry.Command,
			})
			entriesAppended++
			needRollback = true
		}
	}

	if len(req.Entries) > 0 {
		if entriesAppended > 0 {
			log.Printf("[Raft %d] Appended %d/%d entries from leader (log size: %d)", r.serverId, entriesAppended, len(req.Entries), len(r.log))
		} else {
			log.Printf("[Raft %d] All %d entries already present (heartbeat)", r.serverId, len(req.Entries))
		}
	}

	oldCommitIndex := r.commitIndex
	if req.LeaderCommit > int64(r.commitIndex) {
		lastNewIndex := req.PrevLogIndex + int64(len(req.Entries))
		newCommitIndex := req.LeaderCommit
		if req.LeaderCommit > lastNewIndex {
			newCommitIndex = lastNewIndex
		}

		for i := r.commitIndex + 1; i <= int(newCommitIndex); i++ {
			arrayIndex := i - 1
			if arrayIndex >= len(r.log) {
				break
			}
			if r.log[arrayIndex].Term != r.currentTerm {
				break
			}
			r.commitIndex = i
		}

		if r.commitIndex > oldCommitIndex {
			log.Printf("[Raft %d] Advanced commitIndex: %d -> %d", r.serverId, oldCommitIndex, r.commitIndex)
		}
	}

	if needRollback {
		r.mu.Unlock()
		err := r.persister.Save(r.currentTerm, r.votedFor, r.log)
		r.mu.Lock()

		if err != nil {
			log.Printf("[Raft %d] CRITICAL: Failed to persist state, rolling back: %v", r.serverId, err)
			r.currentTerm = savedCurrentTerm
			r.votedFor = savedVotedFor
			r.log = savedLog
			return nil, fmt.Errorf("failed to persist state: %w", err)
		}
	}

	reply.Success = true
	return reply, nil
}

// ReplicateCommand submits a command to the Raft log.
// Only the leader accepts commands. Non-leaders return ErrNotLeader.
// Blocks until the command is committed or timeout occurs (5 seconds).
// Returns the log index even on timeout (client can poll for commit status).
//
// Errors:
//   - ErrNotLeader: This node is not the leader
//   - ErrTimeout: Command not committed within timeout (may still commit later)
func (r *Raft) ReplicateCommand(cmd []byte) (int, error) {
	// Check if leader
	r.mu.Lock()
	if r.state != Leader {
		r.mu.Unlock()
		log.Printf("[Raft %d] ReplicateCommand rejected: not leader", r.serverId)
		return 0, ErrNotLeader
	}

	// Calculate next log index
	nextIndex := len(r.log) + 1

	// Append to local log
	entry := LogEntry{
		Index:   int64(nextIndex),
		Term:    r.currentTerm,
		Command: cmd,
	}
	r.log = append(r.log, entry)
	log.Printf("[Raft %d] Appended command to log at index %d (term %d)", r.serverId, nextIndex, r.currentTerm)

	// Persist immediately (critical for durability)
	if err := r.persist(); err != nil {
		r.mu.Unlock()
		return 0, fmt.Errorf("failed to persist: %w", err)
	}

	// Trigger immediate replication to all peers
	for peerId := range r.peers {
		go r.sendAppendEntries(peerId)
	}

	// Remember target index and release lock
	targetIndex := nextIndex
	r.mu.Unlock()

	// Wait for commit with timeout (5 seconds)
	log.Printf("[Raft %d] Waiting for command %d to commit...", r.serverId, targetIndex)
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			log.Printf("[Raft %d] Timeout waiting for commit of index %d", r.serverId, targetIndex)
			return targetIndex, ErrTimeout
		case <-ticker.C:
			r.mu.Lock()
			committed := r.commitIndex >= targetIndex
			r.mu.Unlock()
			if committed {
				log.Printf("[Raft %d] Command %d committed successfully", r.serverId, targetIndex)
				return targetIndex, nil
			}
		}
	}
}

// sendAppendEntries sends AppendEntries RPC to a specific peer.
// It handles log replication, updating matchIndex/nextIndex on success,
// and retrying with decremented nextIndex on log mismatch.
func (r *Raft) sendAppendEntries(peerId int) {
	peer, ok := r.peers[peerId]
	if !ok {
		return
	}

	r.mu.Lock()
	if r.state != Leader {
		r.mu.Unlock()
		return
	}

	// Get nextIndex for this peer
	nextIndex := peer.nextIndex
	if nextIndex < 1 {
		nextIndex = 1
	}

	// Build prevLog info
	prevLogIndex := nextIndex - 1
	prevLogTerm := 0
	if prevLogIndex > 0 && prevLogIndex <= len(r.log) {
		prevLogTerm = r.log[prevLogIndex-1].Term
	}

	// Get entries to send (starting from nextIndex)
	entries := make([]*pb.LogEntry, 0)
	for i := nextIndex; i <= len(r.log); i++ {
		entry := r.log[i-1]
		entries = append(entries, &pb.LogEntry{
			Index:   entry.Index,
			Term:    int64(entry.Term),
			Command: entry.Command,
		})
	}

	args := &pb.AppendEntriesRequest{
		Term:         int64(r.currentTerm),
		LeaderId:     int32(r.serverId),
		PrevLogIndex: int64(prevLogIndex),
		PrevLogTerm:  int64(prevLogTerm),
		Entries:      entries,
		LeaderCommit: int64(r.commitIndex),
	}
	r.mu.Unlock()

	// Make RPC call with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reply, err := peer.raftClient.AppendEntries(ctx, args)
	if err != nil {
		// RPC failed - will retry on next heartbeat
		log.Printf("[Raft %d] AppendEntries RPC to peer %d failed: %v", r.serverId, peerId, err)
		return
	}

	// Handle response
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if we're still leader and term hasn't changed
	if r.state != Leader || r.currentTerm != int(args.Term) {
		return
	}

	// Check for higher term
	if reply.Term > int64(r.currentTerm) {
		log.Printf("[Raft %d] Peer %d has higher term %d, stepping down", r.serverId, peerId, reply.Term)
		r.stepDown(int(reply.Term))
		return
	}

	if reply.Success {
		// Update matchIndex and nextIndex
		newMatchIndex := prevLogIndex + len(entries)
		if newMatchIndex > peer.matchIndex {
			peer.matchIndex = newMatchIndex
			log.Printf("[Raft %d] Peer %d replicated up to index %d", r.serverId, peerId, peer.matchIndex)
		}
		peer.nextIndex = peer.matchIndex + 1

		// Try to advance commit index
		r.updateCommitIndex()
	} else {
		// Log mismatch - decrement nextIndex and retry
		log.Printf("[Raft %d] Peer %d rejected AppendEntries (log mismatch), retrying with nextIndex=%d", r.serverId, peerId, peer.nextIndex-1)
		if peer.nextIndex > 1 {
			peer.nextIndex--
		}
		// Trigger immediate retry
		go r.sendAppendEntries(peerId)
	}
}

// updateCommitIndex finds the highest N where matchIndex[peer] >= N for majority,
// and advances commitIndex if log[N].term == currentTerm.
func (r *Raft) updateCommitIndex() {
	if r.state != Leader {
		return
	}

	oldCommitIndex := r.commitIndex

	// Find highest N where a majority has matchIndex >= N
	for n := r.commitIndex + 1; n <= len(r.log); n++ {
		// Count how many peers have replicated entry N
		count := 1 // Leader always has the entry
		for _, peer := range r.peers {
			if peer.matchIndex >= n {
				count++
			}
		}

		// Check if we have majority
		if count > len(r.peers)/2 {
			// Only commit if entry is from current term (Raft safety)
			if r.log[n-1].Term == r.currentTerm {
				r.commitIndex = n
				// Signal apply goroutine
				r.applyCond.Broadcast()
				// Signal any waiters for replication
				r.replicationCond.Broadcast()
			}
		} else {
			// No majority for this entry or higher
			break
		}
	}

	if r.commitIndex > oldCommitIndex {
		log.Printf("[Raft %d] Leader advanced commitIndex: %d -> %d (majority achieved)", r.serverId, oldCommitIndex, r.commitIndex)
	}
}
