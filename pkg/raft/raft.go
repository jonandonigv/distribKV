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
	state       State
	commitIndex int
	lastApplied int

	// Leader state
	nextIndex  []int
	matchIndex []int
}
