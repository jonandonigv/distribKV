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

	// If leader's term is lower, reject
	if req.Term < int64(r.currentTerm) {
		return reply, nil
	}

	// If leader's term is higher, update our term and convert to follower
	if req.Term > int64(r.currentTerm) {
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		reply.Term = req.Term
	}

	// Track the leader (valid AppendEntries with current or higher term)
	r.leaderId = int(req.LeaderId)

	// Reset election timer - we've received valid heartbeat/append from leader
	r.resetElectionTimer()

	// Check if prevLogIndex matches
	// prevLogIndex of 0 means leader has no previous log entry (empty log)
	if req.PrevLogIndex > 0 {
		// Check if we have the entry at prevLogIndex
		if int(req.PrevLogIndex) > len(r.log) {
			// We don't have enough entries
			return reply, nil
		}
		// Check if term matches at prevLogIndex (convert 1-based to 0-based array index)
		if r.log[req.PrevLogIndex-1].Term != int(req.PrevLogTerm) {
			// Log conflict - delete this and all following entries
			r.log = r.log[:req.PrevLogIndex-1]
			return reply, nil
		}
	}

	// Append new entries
	// logIndex is 1-based, so we convert to 0-based array index when accessing r.log
	for i, entry := range req.Entries {
		logIndex := int(req.PrevLogIndex) + i // 1-based index of where this entry should go
		arrayIndex := logIndex - 1            // 0-based index in the array

		if arrayIndex < len(r.log) {
			// We have an entry at this index - check for conflict
			if r.log[arrayIndex].Term != int(entry.Term) {
				// Conflict found - delete this and all following entries
				r.log = r.log[:arrayIndex]
				// Append the new entry
				r.log = append(r.log, LogEntry{
					Index:   entry.Index,
					Term:    int(entry.Term),
					Command: entry.Command,
				})
			}
			// If terms match, entry is already there, skip
		} else {
			// Append new entry
			r.log = append(r.log, LogEntry{
				Index:   entry.Index,
				Term:    int(entry.Term),
				Command: entry.Command,
			})
		}
	}

	// Update commitIndex if leaderCommit > commitIndex
	// Note: logIndex is 1-based, commitIndex should also be 1-based
	// SAFETY: Raft never commits log entries from previous terms by counting replicas.
	// We only advance commitIndex if the entry at that index is from the current term.
	if req.LeaderCommit > int64(r.commitIndex) {
		lastNewIndex := req.PrevLogIndex + int64(len(req.Entries)) // 1-based
		newCommitIndex := req.LeaderCommit
		if req.LeaderCommit > lastNewIndex {
			newCommitIndex = lastNewIndex
		}

		// Only commit entries from the current term
		// Entries from previous terms can only be committed indirectly once
		// an entry from the current term is committed (Log Matching Property)
		for i := r.commitIndex + 1; i <= int(newCommitIndex); i++ {
			arrayIndex := i - 1 // Convert to 0-based
			if arrayIndex >= len(r.log) {
				// Don't have this entry yet, stop here
				break
			}
			if r.log[arrayIndex].Term != r.currentTerm {
				// Entry is from a previous term, don't commit it yet
				// Wait until leader commits an entry from current term
				break
			}
			r.commitIndex = i
		}
	}

	// Persist state before responding (Raft requirement)
	if err := r.persist(); err != nil {
		return nil, fmt.Errorf("failed to persist state: %w", err)
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
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return targetIndex, ErrTimeout
		case <-ticker.C:
			r.mu.Lock()
			committed := r.commitIndex >= targetIndex
			r.mu.Unlock()
			if committed {
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
		r.stepDown(int(reply.Term))
		return
	}

	if reply.Success {
		// Update matchIndex and nextIndex
		newMatchIndex := prevLogIndex + len(entries)
		if newMatchIndex > peer.matchIndex {
			peer.matchIndex = newMatchIndex
		}
		peer.nextIndex = peer.matchIndex + 1

		// Try to advance commit index
		r.updateCommitIndex()
	} else {
		// Log mismatch - decrement nextIndex and retry
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
}
