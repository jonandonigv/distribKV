// Package raft implements the Raft consensus algorithm: leader election,
// log replication, and state-machine application via a commit channel.
//
// Architecture overview (see AGENTS.md "Raft Implementation Notes" for
// the full design):
//
//   - Three goroutines per node: election timer (always), heartbeat sender
//     (leader-only), apply loop (always).
//   - Election timer uses time.AfterFunc + Reset; no channel plumbing.
//   - Synchronization is sync.Mutex + channels (applyCh, commitCh);
//     no sync.Cond anywhere.
//   - Shutdown via context cancellation; Start() creates the ctx,
//     Shutdown() cancels and waits on the WaitGroup.
//   - Constructor does NOT start goroutines and does NOT dial peers.
//     Start() does. Raft owns peer *grpc.ClientConn; Shutdown() closes them.
//
// Wire types live in raft/raftpb (proto-generated); domain LogEntry is
// defined in types.go to avoid embedding protoimpl.MessageState's mutex.
package raft

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonandonigv/distribKV/raft/raftpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// Config holds the parameters needed to construct a Raft node. The
// production binary builds this from cluster.yaml; tests build it
// directly. See AGENTS.md "Lifecycle invariants".
type Config struct {
	// ServerID is this node's opaque id from cluster.yaml. Must be >= 0.
	ServerID int

	// OwnAddr is this node's listen address (host:port). Stored for
	// logging/debugging; the gRPC server itself is owned by server/.
	OwnAddr string

	// Peers maps peer id -> peer address, excluding this node. Keys
	// must not include ServerID (validated).
	Peers map[int]string

	// ElectionTimeoutMin/Max bound the randomized election timeout.
	// Heartbeats reset the timer; firing triggers becomeCandidate.
	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration

	// HeartbeatInterval is how often the leader sends AppendEntries.
	HeartbeatInterval time.Duration

	// SnapshotThreshold is parsed from config but unused in 0.1.0
	// (snapshotting is deferred; seam baked in for forward compat).
	SnapshotThreshold int

	// Persister durably stores currentTerm, votedFor, and log. Must
	// be non-nil; tests inject MemoryPersister or fault-injecting variants.
	Persister Persister

	// Logger must be non-nil or slog.Default() is used. Each Raft labels
	// every line with its node id.
	Logger *slog.Logger
}

// peer is one remote node in the cluster. Raft owns the peer's
// *grpc.ClientConn and closes it on Shutdown. Lazy dialing happens
// via ensureConnected on first RPC failure (see AGENTS.md gRPC Guidelines).
type peer struct {
	id      int
	address string

	conn       *grpc.ClientConn
	raftClient raftpb.RaftClient

	// Leader-side replication state. Absolute Raft indices.
	nextIndex  int
	matchIndex int

	// Last successful contact; used for diagnostics. Updated under
	// Raft.mu when AppendEntries/RequestVote replies arrive.
	lastContact time.Time
}

// Raft is one node in the consensus cluster. Construct with NewRaft;
// start goroutines with Start; stop with Shutdown.
type Raft struct {
	// gRPC service embed so *Raft satisfies raftpb.RaftServer.
	raftpb.UnimplementedRaftServer

	mu       sync.Mutex
	serverId int
	address  string
	peers    map[int]*peer

	// Persistent state (saved via persister).
	currentTerm int
	votedFor    int // -1 = no vote this term
	log         []LogEntry
	logBase     int // always 0 in 0.1.0; see AGENTS.md "Identity & Persistence"

	// Volatile state.
	state       State
	commitIndex int
	lastApplied int
	leaderId    int // -1 if unknown; own id when leader

	// Election timer (AfterFunc-based; see AGENTS.md goroutine model).
	electionTimer      *time.Timer
	electionTimeoutMin time.Duration
	electionTimeoutMax time.Duration

	// Deterministic-timeout testing seam. When true, the election timer
	// uses deterministicTimeout instead of a randomized value.
	useDeterministicTimeout bool
	deterministicTimeout    time.Duration

	// Heartbeat sender (leader-only). ticker is nil until becomeLeader.
	heartbeatInterval time.Duration

	// Vote counting during election (only meaningful while state == Candidate).
	votesReceived int

	// Apply pipeline: applyCh delivers committed entries to the KV
	// state machine; commitCh wakes the apply loop when commitIndex
	// advances. Both are allocated in NewRaft.
	applyCh  chan ApplyMsg // buffered 100
	commitCh chan struct{} // buffered 1; dropped signals are harmless

	// Persister for durable state.
	persister Persister

	// Snapshot config knob (parsed but unused in 0.1.0).
	snapshotThreshold int

	// Lifecycle: ctx is created in Start; Shutdown cancels and waits.
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	shutdown atomic.Bool

	// Per-node structured logger.
	logger *slog.Logger
}

// NewRaft constructs a Raft node WITHOUT starting goroutines or dialing
// peers. Call Start() to begin the election timer / apply loop; call
// Shutdown() to stop them. Persisted state is loaded from the persister
// during construction so the returned node reflects any prior incarnation.
func NewRaft(cfg Config) (*Raft, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("raft config: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.Int("node", cfg.ServerID))

	r := &Raft{
		serverId:           cfg.ServerID,
		address:            cfg.OwnAddr,
		peers:              make(map[int]*peer, len(cfg.Peers)),
		votedFor:           -1,
		state:              Follower,
		leaderId:           -1,
		log:                make([]LogEntry, 0),
		electionTimeoutMin: cfg.ElectionTimeoutMin,
		electionTimeoutMax: cfg.ElectionTimeoutMax,
		heartbeatInterval:  cfg.HeartbeatInterval,
		applyCh:            make(chan ApplyMsg, 100),
		commitCh:           make(chan struct{}, 1),
		persister:          cfg.Persister,
		snapshotThreshold:  cfg.SnapshotThreshold,
		logger:             logger,
	}

	for id, addr := range cfg.Peers {
		r.peers[id] = &peer{
			id:        id,
			address:   addr,
			nextIndex: 1,
		}
	}

	// Load any persisted state before returning so the caller sees the
	// restored term/votedFor/log. A missing-file error (fresh node) is
	// benign — we keep the zero state initialized above.
	if err := r.loadPersistedState(); err != nil {
		logger.Debug("no persisted state loaded; starting fresh", slog.String("err", err.Error()))
	} else {
		logger.Debug("loaded persisted state",
			slog.Int("term", r.currentTerm),
			slog.Int("voted_for", r.votedFor),
			slog.Int("log_len", len(r.log)))
	}

	return r, nil
}

// validate enforces Config invariants. See AGENTS.md "Lifecycle invariants".
func (c Config) validate() error {
	if c.ServerID < 0 {
		return fmt.Errorf("server_id must be non-negative (got %d)", c.ServerID)
	}
	if c.Persister == nil {
		return fmt.Errorf("persister is required")
	}
	if _, ok := c.Peers[c.ServerID]; ok {
		return fmt.Errorf("peers map must not include self (id %d)", c.ServerID)
	}
	if c.ElectionTimeoutMin <= 0 {
		return fmt.Errorf("election_timeout_min must be positive (got %s)", c.ElectionTimeoutMin)
	}
	if c.ElectionTimeoutMax <= 0 {
		return fmt.Errorf("election_timeout_max must be positive (got %s)", c.ElectionTimeoutMax)
	}
	if c.ElectionTimeoutMin >= c.ElectionTimeoutMax {
		return fmt.Errorf("election_timeout_min must be less than election_timeout_max (got %s >= %s)",
			c.ElectionTimeoutMin, c.ElectionTimeoutMax)
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("heartbeat_interval must be positive (got %s)", c.HeartbeatInterval)
	}
	return nil
}

// loadPersistedState restores currentTerm, votedFor, and log from the
// persister. On a non-empty log, commitIndex and lastApplied are snapped
// to the last entry's index so we don't replay already-applied entries.
// A fresh-node "does not exist" error is NOT propagated (caller treats
// any error as "start fresh").
func (r *Raft) loadPersistedState() error {
	term, votedFor, log, err := r.persister.Load()
	if err != nil {
		return err
	}
	r.currentTerm = term
	r.votedFor = votedFor
	r.log = log
	if len(log) > 0 {
		r.commitIndex = int(log[len(log)-1].Index)
		r.lastApplied = r.commitIndex
	}
	return nil
}

// ---------------------------------------------------------------------------
// Trivial accessors (whitebox tests + production code call these).
// ---------------------------------------------------------------------------

// GetServerId returns this node's id.
func (r *Raft) GetServerId() int { return r.serverId }

// GetAddress returns this node's listen address.
func (r *Raft) GetAddress() string { return r.address }

// GetPeerCount returns the number of remote peers (excludes self).
func (r *Raft) GetPeerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

// IsLeader reports whether this node is currently the leader.
func (r *Raft) IsLeader() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == Leader
}

// GetLeaderId returns the current leader's id, or this node's id if it
// is the leader, or -1 if unknown.
func (r *Raft) GetLeaderId() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == Leader {
		return r.serverId
	}
	return r.leaderId
}

// GetApplyCh returns the channel that delivers committed entries to the
// KV state machine. Consumers must read from it or the apply loop blocks.
func (r *Raft) GetApplyCh() chan ApplyMsg { return r.applyCh }

// SnapshotThreshold returns the snapshot-threshold config knob (unused
// in 0.1.0; parsed for forward compatibility with the snapshot phase).
func (r *Raft) SnapshotThreshold() int { return r.snapshotThreshold }

// Logger returns this node's structured logger. Exposed for tests that
// want to assert log output (and for adjacent packages sharing the logger).
func (r *Raft) Logger() *slog.Logger { return r.logger }

// ---------------------------------------------------------------------------
// Whitebox accessors used by *_internal_test.go. These are deliberately
// not on the public surface; they exist so tests can assert private state
// without reaching into struct fields directly. Marked as test helpers
// by convention (no caller outside _test.go should use them).
// ---------------------------------------------------------------------------

// State returns the current Raft role. Test helper.
func (r *Raft) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// CurrentTerm returns the current term. Test helper.
func (r *Raft) CurrentTerm() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentTerm
}

// VotedFor returns who this node voted for in the current term (-1 = none). Test helper.
func (r *Raft) VotedFor() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.votedFor
}

// CommitIndex returns the commit index. Test helper.
func (r *Raft) CommitIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitIndex
}

// LastApplied returns the last-applied index. Test helper.
func (r *Raft) LastApplied() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastApplied
}

// LogBase returns the absolute index of log[0] (always 0 in 0.1.0). Test helper.
func (r *Raft) LogBase() int { return r.logBase }

// Log returns a copy of the current log. Test helper; callers must not
// mutate the returned slice (it's a snapshot).
func (r *Raft) Log() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.log))
	copy(out, r.log)
	return out
}

// ---------------------------------------------------------------------------
// Lifecycle: SetDeterministicTimeout, Start, Shutdown, applyLoop.
// ---------------------------------------------------------------------------

// SetDeterministicTimeout overrides the randomized election timeout with
// a fixed value. Test-only seam.
func (r *Raft) SetDeterministicTimeout(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.useDeterministicTimeout = true
	r.deterministicTimeout = d
}

// Start launches the election timer and apply loop goroutine. It creates
// the context that Shutdown cancels. Must be called after the gRPC server
// is listening (see AGENTS.md "Lifecycle invariants"). Idempotent.
func (r *Raft) Start() {
	r.mu.Lock()
	if r.ctx != nil {
		r.mu.Unlock()
		return // already started
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	ctx := r.ctx
	// Arm the election timer.
	r.electionTimer = time.AfterFunc(r.electionTimeoutLocked(), func() {
		r.becomeCandidate()
	})
	r.mu.Unlock()

	// Start the apply loop goroutine.
	r.wg.Add(1)
	go r.applyLoop(ctx)
}

// Shutdown stops all goroutines (election timer, heartbeat sender, apply
// loop) and closes peer gRPC connections. Blocks until all goroutines
// have exited. Safe to call multiple times.
func (r *Raft) Shutdown() {
	if !r.shutdown.CompareAndSwap(false, true) {
		return // already shutting down
	}

	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	if r.electionTimer != nil {
		r.electionTimer.Stop()
	}
	// Close peer connections so any in-flight RPCs fail fast.
	for _, p := range r.peers {
		if p.conn != nil {
			_ = p.conn.Close()
			p.conn = nil
			p.raftClient = nil
		}
	}
	r.mu.Unlock()

	r.wg.Wait()
}

// applyLoop is the always-running goroutine that sends committed log
// entries to the consumer via applyCh. It wakes on commitCh signals;
// dropped signals are harmless because it re-reads commitIndex under
// the mutex.
func (r *Raft) applyLoop(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.commitCh:
			r.applyPendingEntries(ctx)
		}
	}
}

// applyPendingEntries sends all un-applied committed entries to applyCh.
// It stops when lastApplied catches up to commitIndex or when ctx is
// cancelled (shutdown).
func (r *Raft) applyPendingEntries(ctx context.Context) {
	for {
		r.mu.Lock()
		if r.lastApplied >= r.commitIndex {
			r.mu.Unlock()
			return
		}
		r.lastApplied++
		idx := r.lastApplied
		arrayIdx := idx - r.logBase - 1
		if arrayIdx < 0 || arrayIdx >= len(r.log) {
			r.lastApplied--
			r.mu.Unlock()
			return
		}
		entry := r.log[arrayIdx]
		msg := ApplyMsg{
			CommandValid: true,
			Command:      append([]byte(nil), entry.Command...),
			CommandIndex: int(entry.Index),
		}
		r.mu.Unlock()

		select {
		case r.applyCh <- msg:
		case <-ctx.Done():
			// Undo the increment so the next Start can re-apply.
			r.mu.Lock()
			r.lastApplied--
			r.mu.Unlock()
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Internal helpers (called under r.mu unless noted).
// ---------------------------------------------------------------------------

// electionTimeoutLocked returns a randomized timeout in [min, max) or the
// deterministic value if SetDeterministicTimeout was called. Caller must
// hold r.mu.
func (r *Raft) electionTimeoutLocked() time.Duration {
	if r.useDeterministicTimeout {
		return r.deterministicTimeout
	}
	spread := int(r.electionTimeoutMax - r.electionTimeoutMin)
	return r.electionTimeoutMin + time.Duration(rand.Intn(spread))
}

// resetElectionTimerLocked arms the election timer with a fresh timeout.
// Caller must hold r.mu.
func (r *Raft) resetElectionTimerLocked() {
	if r.electionTimer != nil {
		r.electionTimer.Reset(r.electionTimeoutLocked())
	}
}

// getLastLogInfoLocked returns the absolute index and term of the last
// log entry, or (0, 0) if the log is empty. Caller must hold r.mu.
func (r *Raft) getLastLogInfoLocked() (int, int) {
	if len(r.log) == 0 {
		return 0, 0
	}
	last := r.log[len(r.log)-1]
	return int(last.Index), int(last.Term)
}

// persist saves currentTerm, votedFor, and log via the persister. Caller
// must hold r.mu (the persister has its own mutex so no deadlock).
func (r *Raft) persist() error {
	return r.persister.Save(r.currentTerm, r.votedFor, r.log)
}

// ---------------------------------------------------------------------------
// Peer connection management.
// ---------------------------------------------------------------------------

// ensureConnected lazily dials the peer's gRPC server if no connection
// is established. Uses grpc.NewClient (non-blocking) so the first call
// returns immediately; the actual TCP connect happens in the background
// and the first RPC will block until it succeeds or times out.
// See AGENTS.md gRPC Guidelines.
func (p *peer) ensureConnected(ctx context.Context) error {
	if p.conn != nil && p.raftClient != nil {
		return nil
	}

	kacp := keepalive.ClientParameters{
		Time:                10 * time.Second,
		Timeout:             3 * time.Second,
		PermitWithoutStream: true,
	}

	conn, err := grpc.NewClient(p.address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(kacp),
	)
	if err != nil {
		return fmt.Errorf("dial peer %d at %s: %w", p.id, p.address, err)
	}

	p.conn = conn
	p.raftClient = raftpb.NewRaftClient(conn)
	return nil
}
