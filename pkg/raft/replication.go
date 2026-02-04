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
	}

	// Reset election timeout (we received heartbeat from valid leader)
	// TODO: Implement election timer reset

	// Check if prevLogIndex matches
	// prevLogIndex of -1 means leader has no previous log entry (empty log)
	if req.PrevLogIndex >= 0 {
		// Check if we have the entry at prevLogIndex
		if int(req.PrevLogIndex) >= len(r.log) {
			// We don't have enough entries
			return reply, nil
		}
		// Check if term matches at prevLogIndex
		if r.log[req.PrevLogIndex].term != int(req.PrevLogTerm) {
			// Log conflict - delete this and all following entries
			r.log = r.log[:req.PrevLogIndex]
			return reply, nil
		}
	}

	// Append new entries
	for i, entry := range req.Entries {
		logIndex := int(req.PrevLogIndex) + 1 + i

		if logIndex < len(r.log) {
			// We have an entry at this index - check for conflict
			if r.log[logIndex].term != int(entry.Term) {
				// Conflict found - delete this and all following entries
				r.log = r.log[:logIndex]
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
	if req.LeaderCommit > int64(r.commitIndex) {
		lastNewIndex := req.PrevLogIndex + int64(len(req.Entries))
		if req.LeaderCommit < lastNewIndex {
			r.commitIndex = int(req.LeaderCommit)
		} else {
			r.commitIndex = int(lastNewIndex)
		}
	}

	reply.Success = true
	return reply, nil
}
