package raft

import (
	"context"

	pb "github.com/jonandonigv/distribKV/proto/raft"
)

func (r *Raft) RequestVote(ctx context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reply := &pb.RequestVoteResponse{
		Term:        int64(r.currentTerm),
		VoteGranted: false,
	}

	// If candidate's term is lower, reject
	if req.Term < int64(r.currentTerm) {
		return reply, nil
	}

	// If candidate's term is higher, update our term and reset votedFor
	if req.Term > int64(r.currentTerm) {
		r.currentTerm = int(req.Term)
		r.votedFor = -1
		r.state = Follower
		reply.Term = req.Term
	}

	// Check if we can vote for this candidate
	// Vote if: haven't voted yet, or already voted for this candidate
	if r.votedFor == -1 || r.votedFor == int(req.CandidateId) {
		// Check if candidate's log is at least as up-to-date as ours
		lastLogIndex := len(r.log) - 1
		lastLogTerm := 0
		if lastLogIndex >= 0 {
			lastLogTerm = r.log[lastLogIndex].term
		}

		// Candidate's log is at least as up-to-date if:
		// - Its last entry has a higher term, OR
		// - Same term but log is at least as long
		if req.LastLogTerm > int64(lastLogTerm) ||
			(req.LastLogTerm == int64(lastLogTerm) && req.LastLogIndex >= int64(lastLogIndex)) {
			r.votedFor = int(req.CandidateId)
			reply.VoteGranted = true
		}
	}

	return reply, nil
}
