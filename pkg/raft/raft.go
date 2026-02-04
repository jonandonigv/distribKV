package raft

import (
	"sync"

	"github.com/jonandonigv/distribKV/pkg/common"
)

// Alternative peer tracking approach (Option 2):
// type Peer struct {
// 	id          int
// 	client      *common.Client
// 	address     string
// 	nextIndex   int
// 	matchIndex  int
// 	lastContact time.Time
// }
//
// type Raft struct {
// 	mu       sync.Mutex
// 	peers    map[int]*Peer // Map for O(1) lookup by peer ID
// 	serverId int
//
// 	// Persistent state
// 	currentTerm int
// 	votedFor    int
// 	log         []LogEntry
//
// 	// Volatile state
// 	state       State // Follower, Candidate, Leader
// 	commitIndex int
// 	lastApplied int
// }

type Raft struct {
	mu       sync.Mutex
	peers    []*common.Client
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
