package raft

// TODO: Implement complete log replication mechanism
//
// Missing Components:
// - Leader heartbeat sender (periodic empty AppendEntries)
// - Log entry appending from clients
// - Commit index advancement and notification
// - State machine application
// - Log matching optimization
//
// Replication Flow:
// 1. Client submits command to leader
// 2. Leader appends to local log
// 3. Leader sends AppendEntries to all followers
// 4. Followers acknowledge or reject (log mismatch)
// 5. Leader updates matchIndex on success
// 6. Leader advances commitIndex when majority replicated
// 7. Leader notifies followers of new commitIndex
// 8. All nodes apply committed entries to state machine
//
// Key Methods Needed:
// - StartHeartbeat() - leader periodically sends heartbeats
// - SendAppendEntries(peerId int) - send to specific follower
// - ReplicateCommand(cmd []byte) - client request handler
// - UpdateCommitIndex() - advance commit on majority
// - ApplyCommittedEntries() - apply to state machine

import (
	"context"

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
		// TODO: Reset election timer - we've heard from a valid leader
	}

	// TODO: Persist currentTerm to stable storage before responding
	// (Raft requirement: persist state before responding to RPCs)

	// TODO: Reset election timer - we've received valid heartbeat/append from leader

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
	// CRITICAL BUG: Raft never commits log entries from previous terms by counting replicas
	// We should only update commitIndex for entries from the current term
	// TODO: Fix commit rule - only advance commitIndex for entries from currentTerm
	if req.LeaderCommit > int64(r.commitIndex) {
		lastNewIndex := req.PrevLogIndex + int64(len(req.Entries)) // 1-based
		if req.LeaderCommit < lastNewIndex {
			r.commitIndex = int(req.LeaderCommit)
		} else {
			r.commitIndex = int(lastNewIndex)
		}
	}

	reply.Success = true
	return reply, nil
}

// TODO: Implement StartHeartbeat() method
// - Only called when node becomes leader
// - Send empty AppendEntries to all peers periodically (50-100ms)
// - Prevent followers from starting elections
// - Include commitIndex to notify followers of progress
// - Stop when node steps down from leader

// TODO: Implement sendAppendEntries(peerId int) method
// - Send AppendEntries RPC to specific follower
// - Include prevLogIndex, prevLogTerm for consistency check
// - Send entries starting from nextIndex[peerId]
// - Handle success: update matchIndex, nextIndex
// - Handle failure (log mismatch): decrement nextIndex and retry
// - Called by heartbeat loop and replication trigger

// TODO: Implement replicateCommand(cmd []byte) method
// - Called by KV server when client submits command
// - Only accept if leader
// - Append to local log
// - Trigger immediate replication to all peers
// - Wait for commit or timeout
// - Return success after commit

// TODO: Implement updateCommitIndex() method
// - Called after successful AppendEntries responses
// - Find highest N where matchIndex[peer] >= N for majority
// - Only advance if log[N].term == currentTerm
// - Never commit entries from previous terms by counting
// - Update commitIndex and notify followers
// - Trigger state machine application

// TODO: Implement applyCommittedEntries() method
// - Background goroutine monitoring commitIndex
// - Apply entries from lastApplied+1 to commitIndex
// - Send applied entries to KV server via apply channel
// - Update lastApplied after each successful apply
// - Must be deterministic across all nodes
