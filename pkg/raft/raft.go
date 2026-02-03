package raft

import (
	"sync"

	"google.golang.org/grpc"
)

type Raft struct {
	mu       sync.Mutex
	peers    []*grpc.ClientConn // Subject to change
	serverId int

	// Persistent state
	currentTerm int
	votedFor    int
	log         []LogEntry

	// Volatile state
	state       State // Follower, Candidate, Leader
	commitIndex int
	lastApplied int

	// Leader state
	nextIndex  []int
	matchIndex []int
}

type State int

const (
	Follower State = iota
	Cadidate
	Leader
)

type LogEntry struct {
	index   int64
	term    int
	command []byte
}
