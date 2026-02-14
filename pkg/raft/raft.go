package raft

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	pb "github.com/jonandonigv/distribKV/proto/raft"
)

// Replication errors
var (
	ErrNotLeader = errors.New("not leader")
	ErrTimeout   = errors.New("timeout waiting for commit")
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
	leaderId    int // Current leader ID (-1 if unknown, own ID if leader)

	// Election timer (goroutine + select approach)
	electionResetChan       chan struct{}
	electionStopChan        chan struct{}
	electionTimeoutMin      time.Duration
	electionTimeoutMax      time.Duration
	useDeterministicTimeout bool
	deterministicTimeout    time.Duration

	// Vote counting (only used during candidacy)
	votesReceived int
	votesMutex    sync.Mutex

	// Heartbeat sender (leader only)
	heartbeatStopChan chan struct{}
	heartbeatInterval time.Duration // 50ms

	// Replication coordination
	replicationCond *sync.Cond // Signals when commitIndex advances

	// Apply channel for state machine
	applyCh   chan ApplyMsg // Buffer size: 10
	applyCond *sync.Cond    // Signals apply goroutine

	// Persister for durability
	persister *Persister
}

// ApplyMsg is sent to the application (KV service) when a log entry is committed.
type ApplyMsg struct {
	CommandValid bool
	Command      []byte
	CommandIndex int
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

func NewRaft(serverId int, peerAddresses []string, connectCtx context.Context) (*Raft, error) {
	r := &Raft{
		serverId:                serverId,
		peers:                   make(map[int]*Peer),
		votedFor:                -1,
		state:                   Follower,
		commitIndex:             0,
		lastApplied:             0,
		leaderId:                -1, // Unknown on startup
		log:                     make([]LogEntry, 0),
		electionTimeoutMin:      150 * time.Millisecond,
		electionTimeoutMax:      300 * time.Millisecond,
		useDeterministicTimeout: false,
		heartbeatInterval:       50 * time.Millisecond,
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

	// Establish connections to all peers
	if err := r.ConnectPeers(connectCtx); err != nil {
		return nil, err
	}

	// Initialize persister and load persisted state
	dataDir := "./data"
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	r.persister = NewPersister(filepath.Join(dataDir, "raft-state.json"))

	// Try to load existing state
	if err := r.loadPersistedState(); err != nil {
		// If load fails, start fresh but log warning
		log.Printf("Warning: Could not load persisted state for server %d: %v", serverId, err)
	}

	// Initialize apply channel and conditions
	r.applyCh = make(chan ApplyMsg, 10)
	r.replicationCond = sync.NewCond(&r.mu)
	r.applyCond = sync.NewCond(&r.mu)

	// Start apply goroutine
	go r.applyCommittedEntries()

	return r, nil
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

// IsLeader returns true if this node is the current leader.
func (r *Raft) IsLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == Leader
}

// GetLeaderId returns the current leader's ID.
// Returns own ID if this node is leader, -1 if unknown.
func (r *Raft) GetLeaderId() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == Leader {
		return r.serverId
	}
	return r.leaderId
}

// ConnectPeers establishes gRPC connections to all peers.
// This method attempts to connect to all peers but allows some to fail initially.
// Failed connections will be retried on-demand during operation.
func (r *Raft) ConnectPeers(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var wg sync.WaitGroup
	var successCount int32
	var failCount int32

	for peerId, peer := range r.peers {
		wg.Add(1)
		go func(id int, p *Peer) {
			defer wg.Done()

			// Create client
			p.client = common.NewClient(p.address)

			// Connect with timeout (10 seconds per peer)
			connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			if err := p.client.Connect(connectCtx); err != nil {
				log.Printf("Warning: Failed to connect to peer %d at %s: %v (will retry later)", id, p.address, err)
				atomic.AddInt32(&failCount, 1)
				return
			}

			// Create Raft client for RPC calls
			p.raftClient = pb.NewRaftClient(p.client.Conn())

			// Verify connection by making a lightweight RPC call
			rpcCtx, rpcCancel := context.WithTimeout(ctx, 5*time.Second)
			defer rpcCancel()

			_, err := p.raftClient.RequestVote(rpcCtx, &pb.RequestVoteRequest{
				Term:         0,
				CandidateId:  int32(r.serverId),
				LastLogIndex: 0,
				LastLogTerm:  0,
			})

			if err != nil && isConnectionError(err) {
				log.Printf("Warning: Failed to reach peer %d at %s: %v (will retry later)", id, p.address, err)
				atomic.AddInt32(&failCount, 1)
				return
			}

			atomic.AddInt32(&successCount, 1)
		}(peerId, peer)
	}

	// Wait for all connection attempts to complete
	wg.Wait()

	// Log summary
	if failCount > 0 {
		log.Printf("Connected to %d/%d peers (%d failed, will retry on-demand)", successCount, len(r.peers), failCount)
	} else {
		log.Printf("Connected to all %d peers", len(r.peers))
	}

	return nil
}

// ensureConnected checks if a peer connection is healthy and reconnects if needed.
// This provides auto-retry functionality for failed connections.
func (p *Peer) ensureConnected(ctx context.Context) error {
	if p.client != nil && p.raftClient != nil {
		// TODO: Add health check to verify connection is still alive
		// For now, assume connection is good if initialized
		return nil
	}

	// Connection lost or never established, reconnect
	p.client = common.NewClient(p.address)

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := p.client.Connect(connectCtx); err != nil {
		return fmt.Errorf("failed to reconnect to peer %d at %s: %w", p.id, p.address, err)
	}

	p.raftClient = pb.NewRaftClient(p.client.Conn())
	return nil
}

// isConnectionError checks if an error is a connection-related error
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// Check for common connection error patterns
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded")
}

// Start initializes and starts the election timer goroutine.
// Must be called after NewRaft() to begin the election process.
func (r *Raft) Start() {
	r.electionResetChan = make(chan struct{}, 10)
	r.electionStopChan = make(chan struct{})
	go r.runElectionTimer()
}

// SetDeterministicTimeout sets a fixed timeout for testing purposes.
// When enabled, the election timer will use this fixed value instead of randomization.
func (r *Raft) SetDeterministicTimeout(timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.useDeterministicTimeout = true
	r.deterministicTimeout = timeout
}

// getLastLogInfo returns the index and term of the last log entry.
// Returns (0, 0) if log is empty.
func (r *Raft) getLastLogInfo() (index int, term int) {
	if len(r.log) == 0 {
		return 0, 0
	}
	lastEntry := r.log[len(r.log)-1]
	return int(lastEntry.Index), lastEntry.Term
}

// majorityCount returns the number of votes needed for a majority.
func (r *Raft) majorityCount() int {
	return (len(r.peers)+1)/2 + 1
}

// applyCommittedEntries is a background goroutine that applies committed log entries
// to the state machine. It runs continuously and sends applied entries via applyCh.
func (r *Raft) applyCommittedEntries() {
	for {
		r.mu.Lock()

		// Wait until there are entries to apply
		for r.lastApplied >= r.commitIndex {
			r.applyCond.Wait()
		}

		// Get entries to apply
		entriesToApply := make([]LogEntry, 0)
		for i := r.lastApplied + 1; i <= r.commitIndex; i++ {
			arrayIndex := i - 1 // Convert to 0-based
			if arrayIndex < len(r.log) {
				entriesToApply = append(entriesToApply, r.log[arrayIndex])
			}
		}
		r.mu.Unlock()

		// Apply entries outside of lock
		for _, entry := range entriesToApply {
			msg := ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandIndex: int(entry.Index),
			}

			// Send to apply channel (blocking)
			r.applyCh <- msg

			// Update lastApplied
			r.mu.Lock()
			r.lastApplied = int(entry.Index)
			r.mu.Unlock()
		}
	}
}

// GetApplyCh returns the channel for receiving applied log entries.
// The application (KV service) should read from this channel.
func (r *Raft) GetApplyCh() chan ApplyMsg {
	return r.applyCh
}

// loadPersistedState loads state from disk if it exists.
// If no persisted state exists, starts fresh.
func (r *Raft) loadPersistedState() error {
	currentTerm, votedFor, log, err := r.persister.Load()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.currentTerm = currentTerm
	r.votedFor = votedFor
	r.log = log

	// Initialize commitIndex and lastApplied based on loaded log
	if len(r.log) > 0 {
		r.lastApplied = int(r.log[len(r.log)-1].Index)
		r.commitIndex = r.lastApplied
	}

	return nil
}

// persist saves the current state to disk.
// Must be called before responding to any RPC.
func (r *Raft) persist() error {
	r.mu.Lock()
	currentTerm := r.currentTerm
	votedFor := r.votedFor
	log := r.log
	r.mu.Unlock()

	return r.persister.Save(currentTerm, votedFor, log)
}
