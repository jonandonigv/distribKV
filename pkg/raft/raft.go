package raft

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	pb "github.com/jonandonigv/distribKV/proto/raft"
)

type Peer struct {
	id          int
	address     string
	client      *common.Client
	raftClient  pb.RaftClient
	nextIndex   int
	matchIndex  int
	lastContact time.Time
}

type Raft struct {
	mu       sync.Mutex
	peers    map[int]*Peer
	serverId int
	address  string

	// Persistent state
	currentTerm int
	votedFor    int
	log         []LogEntry

	// Volatile state
	state       State
	commitIndex int
	lastApplied int

	// TODO: Election timer - randomized timeout (150-300ms)
	// TODO: Heartbeat ticker - leader sends AppendEntries periodically
	// TODO: Apply channel - for committing entries to state machine
}

type State int

const (
	Follower State = iota
	Candidate
	Leader
)

type LogEntry struct {
	Index   int64
	Term    int
	Command []byte
}

func deriveIdFromAddress(address string) int {
	parts := strings.Split(address, ":")
	if len(parts) != 2 {
		return 0
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return port % 10000
}

func NewRaft(serverId int, peerAddresses []string) *Raft {
	r := &Raft{
		serverId:    serverId,
		peers:       make(map[int]*Peer),
		votedFor:    -1,
		state:       Follower,
		commitIndex: 0,
		lastApplied: 0,
		log:         make([]LogEntry, 0),
	}

	for _, addr := range peerAddresses {
		peerId := deriveIdFromAddress(addr)
		if peerId == 0 {
			continue
		}
		if peerId == serverId {
			r.address = addr
			continue
		}

		r.peers[peerId] = &Peer{
			id:          peerId,
			address:     addr,
			nextIndex:   1,
			matchIndex:  0,
			lastContact: time.Time{},
		}
	}

	return r
}

func (r *Raft) GetPeerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

func (r *Raft) GetServerId() int {
	return r.serverId
}

func (r *Raft) GetAddress() string {
	return r.address
}

// TODO: Add ConnectPeers() - establish gRPC connections to all peers

// TODO: Add StartElectionTimer() - begin randomized timeout for leader election

// TODO: Add sendAppendEntries(peerId int) - send heartbeat/log entries to specific peer

// TODO: Add sendRequestVote(peerId int) - request vote during election

// TODO: Add applyLogEntries() - apply committed entries to state machine

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
