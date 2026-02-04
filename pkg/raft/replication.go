package raft

import (
	"context"

	pb "github.com/jonandonigv/distribKV/proto/raft"
)

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
		if r.log[req.PrevLogIndex-1].term != int(req.PrevLogTerm) {
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
			if r.log[arrayIndex].term != int(entry.Term) {
				// Conflict found - delete this and all following entries
				r.log = r.log[:arrayIndex]
				// Append the new entry
				r.log = append(r.log, LogEntry{
					index:   entry.Index,
					term:    int(entry.Term),
					command: entry.Command,
				})
			}
			// If terms match, entry is already there, skip
		} else {
			// Append new entry
			r.log = append(r.log, LogEntry{
				index:   entry.Index,
				term:    int(entry.Term),
				command: entry.Command,
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
