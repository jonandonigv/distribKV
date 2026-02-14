package kvserver

import (
	"context"
	"log"
	"time"

	"github.com/jonandonigv/distribKV/pkg/raft"
	pb "github.com/jonandonigv/distribKV/proto/kv"
)

// NewKVServer creates a new KVServer instance
func NewKVServer(rf *raft.Raft, maxPendingOps int) *KVServer {
	if maxPendingOps == 0 {
		maxPendingOps = DefaultMaxPendingOps
	}

	kv := &KVServer{
		rf:            rf,
		applyCh:       rf.GetApplyCh(),
		state:         make(map[string]string),
		duplicates:    make(map[int64]map[int64]*DuplicateEntry),
		pendingOps:    make(map[int]*PendingOp),
		maxPendingOps: maxPendingOps,
		shutdownCh:    make(chan struct{}),
	}

	// Start apply loop
	go kv.applyLoop()
	log.Printf("[KV] KVServer created with maxPendingOps=%d", maxPendingOps)

	return kv
}

// Get handles Get RPC requests
func (kv *KVServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	// Check leadership
	if !kv.rf.IsLeader() {
		log.Printf("[KV] Get rejected: not leader (client=%d)", req.ClientId)
		return &pb.GetResponse{
			Success:     false,
			WrongLeader: true,
			LeaderId:    int32(kv.rf.GetLeaderId()),
		}, nil
	}

	// Check duplicate cache (including Get results for consistency)
	kv.mu.Lock()
	if dup := kv.getDuplicate(req.ClientId, req.SequenceNum); dup != nil {
		kv.mu.Unlock()
		log.Printf("[KV] Get cache hit: client=%d seq=%d", req.ClientId, req.SequenceNum)
		return &pb.GetResponse{
			Success: true,
			Value:   dup.Result.Value,
		}, nil
	}
	kv.mu.Unlock()

	// Create operation
	op := Op{
		Type:       OpGet,
		Key:        req.Key,
		ClientId:   req.ClientId,
		SequenceId: req.SequenceNum,
	}

	// Submit and wait
	result, err := kv.submitOperation(op)
	if err != nil {
		log.Printf("[KV] Get failed for key=%s: %v", req.Key, err)
		return &pb.GetResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	log.Printf("[KV] Get success: key=%s, value=%s", req.Key, result.Value)
	return &pb.GetResponse{
		Success: true,
		Value:   result.Value,
	}, nil
}

// Put handles Put RPC requests
func (kv *KVServer) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	// Check leadership
	if !kv.rf.IsLeader() {
		log.Printf("[KV] Put rejected: not leader (client=%d)", req.ClientId)
		return &pb.PutResponse{
			Success:     false,
			WrongLeader: true,
			LeaderId:    int32(kv.rf.GetLeaderId()),
		}, nil
	}

	// Check duplicate cache
	kv.mu.Lock()
	if dup := kv.getDuplicate(req.ClientId, req.SequenceNum); dup != nil {
		kv.mu.Unlock()
		log.Printf("[KV] Put cache hit: client=%d seq=%d", req.ClientId, req.SequenceNum)
		return &pb.PutResponse{Success: true}, nil
	}
	kv.mu.Unlock()

	// Create operation
	op := Op{
		Type:       OpPut,
		Key:        req.Key,
		Value:      req.Value,
		ClientId:   req.ClientId,
		SequenceId: req.SequenceNum,
	}

	// Submit and wait
	_, err := kv.submitOperation(op)
	if err != nil {
		log.Printf("[KV] Put failed for key=%s: %v", req.Key, err)
		return &pb.PutResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	log.Printf("[KV] Put success: key=%s", req.Key)
	return &pb.PutResponse{Success: true}, nil
}

// Append handles Append RPC requests
func (kv *KVServer) Append(ctx context.Context, req *pb.AppendRequest) (*pb.AppendResponse, error) {
	// Check leadership
	if !kv.rf.IsLeader() {
		log.Printf("[KV] Append rejected: not leader (client=%d)", req.ClientId)
		return &pb.AppendResponse{
			Success:     false,
			WrongLeader: true,
			LeaderId:    int32(kv.rf.GetLeaderId()),
		}, nil
	}

	// Check duplicate cache
	kv.mu.Lock()
	if dup := kv.getDuplicate(req.ClientId, req.SequenceNum); dup != nil {
		kv.mu.Unlock()
		log.Printf("[KV] Append cache hit: client=%d seq=%d", req.ClientId, req.SequenceNum)
		return &pb.AppendResponse{Success: true}, nil
	}
	kv.mu.Unlock()

	// Create operation
	op := Op{
		Type:       OpAppend,
		Key:        req.Key,
		Value:      req.Value,
		ClientId:   req.ClientId,
		SequenceId: req.SequenceNum,
	}

	// Submit and wait
	_, err := kv.submitOperation(op)
	if err != nil {
		log.Printf("[KV] Append failed for key=%s: %v", req.Key, err)
		return &pb.AppendResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	log.Printf("[KV] Append success: key=%s", req.Key)
	return &pb.AppendResponse{Success: true}, nil
}

// submitOperation submits an operation to Raft and waits for it to commit
func (kv *KVServer) submitOperation(op Op) (Result, error) {
	// Check pending limit
	kv.mu.Lock()
	if len(kv.pendingOps) >= kv.maxPendingOps {
		kv.mu.Unlock()
		log.Printf("[KV] Operation rejected: too many pending operations (%d)", len(kv.pendingOps))
		return Result{}, ErrTooManyPending
	}
	kv.mu.Unlock()

	// Serialize command
	data, err := kv.serializeCommand(op)
	if err != nil {
		return Result{}, err
	}

	// Submit to Raft
	index, err := kv.rf.ReplicateCommand(data)
	if err != nil {
		return Result{}, err // ErrNotLeader or ErrTimeout from Raft
	}

	// Register pending operation
	resultCh := make(chan Result, 1)

	kv.mu.Lock()
	kv.pendingOps[index] = &PendingOp{
		Index:    index,
		Op:       op,
		ResultCh: resultCh,
	}
	kv.mu.Unlock()

	// Wait with timeout
	select {
	case result := <-resultCh:
		return result, nil
	case <-kv.shutdownCh:
		// Server shutting down
		kv.mu.Lock()
		delete(kv.pendingOps, index)
		kv.mu.Unlock()
		return Result{}, ErrTimeout
	case <-time.After(RPCTimeout):
		kv.mu.Lock()
		delete(kv.pendingOps, index)
		kv.mu.Unlock()
		log.Printf("[KV] Operation timeout waiting for index %d", index)
		return Result{}, ErrTimeout
	}
}

// Kill is used for testing to shut down the server
func (kv *KVServer) Kill() {
	log.Printf("[KV] KVServer shutting down")
	kv.mu.Lock()
	kv.dead = true
	kv.mu.Unlock()
	close(kv.shutdownCh)
}

// isDead returns true if the server has been killed
func (kv *KVServer) isDead() bool {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.dead
}
