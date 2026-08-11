// Server RPC handlers: Get/Put/Append. Each handler constructs a Command,
// submits it to raft via ReplicateCommand, and waits for the apply loop
// to deliver the result. Non-leader returns wrong_leader + leader_id.
//
// See AGENTS.md "KV Service Notes".

package kv

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jonandonigv/distribKV/raft"
)

// NewServer constructs a KV server on top of the given raft node.
// It starts the apply loop goroutine immediately. Use Kill to stop it.
//
// maxPending is the cap on in-flight RPC handlers waiting for raft to
// commit; above this cap, submitOperation returns ErrTooManyPending.
// Pass DefaultMaxPendingOps for the standard value.
func NewServer(rf *raft.Raft, maxPending int, logger *slog.Logger) *Server {
	if maxPending <= 0 {
		maxPending = DefaultMaxPendingOps
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(slog.Int("node", rf.GetServerId()))

	s := &Server{
		rf:         rf,
		applyCh:    rf.GetApplyCh(),
		state:      make(map[string]string),
		duplicates: make(map[int64]map[int64]*DuplicateEntry),
		pendingOps: make(map[int]*PendingOp),
		recent:     make(map[int]Result),
		maxPending: maxPending,
		shutdownCh: make(chan struct{}),
		logger:     logger,
	}
	go s.applyLoop()
	return s
}

// Kill stops the server. It sets dead=true, closes shutdownCh (so
// submitOperation returns ErrShutdown), and drains any buffered entries
// from applyCh so raft's apply loop doesn't block when it tries to send.
// The drain has a bounded timeout (2s) so Kill never hangs.
//
// Production shutdown order: rf.Shutdown() → Kill() → grpcServer.GracefulStop().
func (s *Server) Kill() {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead = true
	close(s.shutdownCh)

	// Notify all pending waiters that the server is shutting down.
	for _, pending := range s.pendingOps {
		select {
		case pending.ResultCh <- Result{Err: ErrShutdown}:
		default:
		}
	}
	s.pendingOps = make(map[int]*PendingOp)
	s.mu.Unlock()

	// Drain buffered entries from applyCh with a bounded timeout so
	// the Raft apply loop can flush remaining committed entries. If
	// no Raft apply loop is running (unit test), this exits quickly
	// on the timeout. This prevents rf.Shutdown()'s WaitGroup.Wait
	// from blocking forever on the Raft apply loop's send to applyCh.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	for {
		select {
		case <-s.applyCh:
		case <-drainCtx.Done():
			return
		}
	}
}

// ---------------------------------------------------------------------------
// submitOperation — the core RPC-to-raft bridge.
// ---------------------------------------------------------------------------

// submitOperation serializes the op, submits it to raft via
// ReplicateCommand, and waits for the apply loop to deliver the result.
// Returns:
//   - (result, nil) on success (committed and applied).
//   - (Result{}, ErrNotLeader) if this node is not the raft leader.
//   - (Result{}, ErrTooManyPending) if the pendingOps map is full.
//   - (Result{}, ErrTimeout) if the 5s timeout fires before the result.
//   - (Result{}, ErrShutdown) if Kill was called while waiting.
//
// For 0.1.0 (no client-supplied IDs yet), ClientId=0 means "no dedup" —
// the dedup cache is skipped so Get and Put don't cross-walk. The
// dedup machinery is exercised by unit tests that set ClientId explicitly.
func (s *Server) submitOperation(op Op) (Result, error) {
	// Serialize the command.
	data, err := serializeCommand(op)
	if err != nil {
		return Result{}, fmt.Errorf("serialize op: %w", err)
	}

	// Check dedup cache BEFORE submitting to raft — but only when the
	// caller provided a real (clientId, seqNum). (0, 0) is the sentinel
	// for "no dedup" so Get and Put don't cross-walk.
	if op.ClientId != 0 && op.SequenceId != 0 {
		s.mu.Lock()
		if dup := s.getDuplicateLocked(op.ClientId, op.SequenceId); dup != nil {
			dupResult := dup.Result
			s.mu.Unlock()
			return dupResult, nil
		}
		s.mu.Unlock()
	}

	// Submit to raft.
	index, err := s.rf.ReplicateCommand(data)
	if err != nil {
		if err == raft.ErrNotLeader {
			return Result{}, ErrNotLeader
		}
		return Result{}, err
	}

	// Register a pending waiter for this index.
	resultCh := make(chan Result, 1)
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return Result{}, ErrShutdown
	}
	if len(s.pendingOps) >= s.maxPending {
		s.mu.Unlock()
		return Result{}, ErrTooManyPending
	}
	s.pendingOps[index] = &PendingOp{
		Index:    index,
		Op:       op,
		ResultCh: resultCh,
	}

	// The apply loop may have already applied this entry and stashed the
	// result in recent (no waiter registered yet at notify time). Check
	// under the same lock so we can't miss it: notifyWaiter either finds
	// the waiter or stashes in recent — never drops.
	if res, ok := s.recent[index]; ok {
		delete(s.recent, index)
		s.mu.Unlock()
		return res, nil
	}
	s.mu.Unlock()

	// Wait for the result or timeout.
	select {
	case result := <-resultCh:
		return result, nil
	case <-time.After(RPCTimeout):
		// Timeout — remove the pending entry.
		s.mu.Lock()
		delete(s.pendingOps, index)
		s.mu.Unlock()
		return Result{}, ErrTimeout
	case <-s.shutdownCh:
		return Result{}, ErrShutdown
	}
}

// ---------------------------------------------------------------------------
// RPC handlers — implement the KV service from kv_grpc.pb.go.
// ---------------------------------------------------------------------------

// Get handles a read request. Goes through the raft log for linearizability.
func (s *Server) Get(ctx context.Context, req *GetRequest) (*GetResponse, error) {
	// Check if we're the leader before doing work.
	if !s.rf.IsLeader() {
		return s.wrongLeaderGet(), nil
	}

	op := Op{
		Type: OpGet,
		Key:  req.GetKey(),
	}

	result, err := s.submitOperation(op)
	if err != nil {
		return s.errorGet(err), nil
	}
	if result.Err != nil {
		if result.Err == ErrKeyNotFound {
			return &GetResponse{
				Success: false,
				Error:   result.Err.Error(),
			}, nil
		}
		return &GetResponse{
			Success: false,
			Error:   result.Err.Error(),
		}, nil
	}

	return &GetResponse{
		Success: true,
		Value:   result.Value,
	}, nil
}

// Put handles a write request.
func (s *Server) Put(ctx context.Context, req *PutRequest) (*PutResponse, error) {
	if !s.rf.IsLeader() {
		return s.wrongLeaderPut(), nil
	}

	op := Op{
		Type:  OpPut,
		Key:   req.GetKey(),
		Value: req.GetValue(),
	}

	_, err := s.submitOperation(op)
	if err != nil {
		return s.errorPut(err), nil
	}

	return &PutResponse{Success: true}, nil
}

// Append handles an append request.
func (s *Server) Append(ctx context.Context, req *AppendRequest) (*AppendResponse, error) {
	if !s.rf.IsLeader() {
		return s.wrongLeaderAppend(), nil
	}

	op := Op{
		Type:  OpAppend,
		Key:   req.GetKey(),
		Value: req.GetValue(),
	}

	_, err := s.submitOperation(op)
	if err != nil {
		return s.errorAppend(err), nil
	}

	return &AppendResponse{Success: true}, nil
}

// ---------------------------------------------------------------------------
// Wrong-leader and error response helpers.
// ---------------------------------------------------------------------------

// wrongLeaderGet builds a GetResponse that redirects the client to the
// current leader. leader_id is set from rf.GetLeaderId() (-1 if unknown).
func (s *Server) wrongLeaderGet() *GetResponse {
	return &GetResponse{
		WrongLeader: true,
		LeaderId:    int32(s.rf.GetLeaderId()),
	}
}

func (s *Server) wrongLeaderPut() *PutResponse {
	return &PutResponse{
		WrongLeader: true,
		LeaderId:    int32(s.rf.GetLeaderId()),
	}
}

func (s *Server) wrongLeaderAppend() *AppendResponse {
	return &AppendResponse{
		WrongLeader: true,
		LeaderId:    int32(s.rf.GetLeaderId()),
	}
}

// errorGet builds a GetResponse for a submitOperation error.
func (s *Server) errorGet(err error) *GetResponse {
	if err == ErrNotLeader {
		return s.wrongLeaderGet()
	}
	return &GetResponse{Success: false, Error: err.Error()}
}

func (s *Server) errorPut(err error) *PutResponse {
	if err == ErrNotLeader {
		return s.wrongLeaderPut()
	}
	return &PutResponse{Success: false, Error: err.Error()}
}

func (s *Server) errorAppend(err error) *AppendResponse {
	if err == ErrNotLeader {
		return s.wrongLeaderAppend()
	}
	return &AppendResponse{Success: false, Error: err.Error()}
}
