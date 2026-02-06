package raft

// TODO: Implement complete leader election mechanism
//
// Missing Components:
// - Election timer with randomized timeout (150-300ms)
// - Timeout expiration triggers candidate transition
// - Candidate increments term, votes for self, sends RequestVote RPCs
// - Vote counting and majority detection
// - Election safety: only one leader per term
//
// Election Flow:
// 1. Follower timer expires -> become Candidate
// 2. Candidate: increment term, vote for self, reset timer
// 3. Send RequestVote to all peers concurrently
// 4. Count votes: if majority -> become Leader
// 5. If higher term seen -> step down to Follower
// 6. If election timeout -> increment term, try again
//
// Key Methods Needed:
// - startElectionTimer() - begin randomized countdown
// - becomeCandidate() - transition to candidate state
// - sendRequestVote(peerId int) - RPC to single peer
// - countVotes() - tally responses and decide outcome

import (
	"context"

	pb "github.com/jonandonigv/distribKV/proto/raft"
)

// RequestVote handles incoming vote requests from candidates.
// Called when another server requests our vote.
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
		// TODO: Reset election timer - we've heard from a valid leader/candidate
	}

	// TODO: Persist currentTerm and votedFor to stable storage before responding
	// (Raft requirement: persist state before responding to RPCs)

	// Check if we can vote for this candidate
	// Vote if: haven't voted yet, or already voted for this candidate
	if r.votedFor == -1 || r.votedFor == int(req.CandidateId) {
		// Check if candidate's log is at least as up-to-date as ours
		// Note: Raft uses 1-based indexing (0 means empty log)
		lastLogIndex := len(r.log) // 0 if empty, otherwise index of last entry
		lastLogTerm := 0
		if lastLogIndex > 0 {
			lastLogTerm = r.log[lastLogIndex-1].Term // Array is 0-based
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

// TODO: Implement sendRequestVote(peerId int) method
// - Called by candidate to request vote from specific peer
// - Should be called concurrently for all peers
// - Handle RPC failures (timeout, network error)
// - Count successful votes toward majority
// - Detect higher terms and step down
// - Race-safe vote counting with mutex

// TODO: Implement startElectionTimer() method
// - Create randomized timeout (150-300ms)
// - Reset on heartbeat received (valid leader)
// - Reset on vote granted (valid candidate)
// - On timeout: becomeCandidate() and start new election
// - Use time.AfterFunc or goroutine with select

// TODO: Implement becomeCandidate() method
// - Increment currentTerm
// - Vote for self
// - Reset election timer
// - Send RequestVote to all peers
// - Transition to Candidate state
// - Start vote counting

// TODO: Implement stepDown(newTerm int) helper
// - Update to higher term
// - Reset votedFor
// - Become Follower
// - Reset election timer
// - Called when higher term discovered

// becomeLeader transitions the node to leader state.
// Called when candidate receives majority of votes.
func (r *Raft) becomeLeader() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.state = Leader

	for _, peer := range r.peers {
		peer.nextIndex = len(r.log) + 1
		peer.matchIndex = 0
	}

	// TODO: Start heartbeat sender goroutine
	// TODO: Stop election timer
}
