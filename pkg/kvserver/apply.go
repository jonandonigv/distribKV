package kvserver

import (
	"fmt"
	"log"
	"time"

	"github.com/jonandonigv/distribKV/pkg/raft"
	pb "github.com/jonandonigv/distribKV/proto/kv"
	"google.golang.org/protobuf/proto"
)

const (
	maxDuplicateEntriesPerClient = 100
	duplicateCacheExpiry         = 10 * time.Second
)

// applyLoop runs as a background goroutine, processing committed Raft commands
func (kv *KVServer) applyLoop() {
	log.Printf("[KV] Apply loop started")
	for {
		select {
		case msg := <-kv.applyCh:
			kv.processApplyMsg(msg)
		case <-kv.shutdownCh:
			log.Printf("[KV] Apply loop shutting down")
			kv.drainApplyCh()
			log.Printf("[KV] Apply loop stopped")
			return
		}
	}
}

// processApplyMsg handles a single committed log entry
func (kv *KVServer) processApplyMsg(msg raft.ApplyMsg) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	cmd, err := kv.deserializeCommand(msg.Command)
	if err != nil {
		log.Printf("[KV] CRITICAL: Failed to deserialize command at index %d: %v. Raw bytes: %x",
			msg.CommandIndex, err, msg.Command)

		kv.notifyWaiter(msg.CommandIndex, Result{Err: fmt.Errorf("command corrupted: %w", err)})
		return
	}

	// Check if this is a duplicate operation
	if dup := kv.getDuplicate(cmd.ClientId, cmd.SequenceId); dup != nil {
		// Already applied, notify waiter if pending
		log.Printf("[KV] Duplicate request detected: client=%d seq=%d, using cached result", cmd.ClientId, cmd.SequenceId)
		kv.notifyWaiter(msg.CommandIndex, dup.Result)
		return
	}

	// Apply command to state machine
	result := kv.applyCommand(cmd)
	log.Printf("[KV] Applied %v operation (index %d): key=%s", cmd.Type, msg.CommandIndex, cmd.Key)

	// Save result to duplicate cache
	kv.saveDuplicate(cmd.ClientId, cmd.SequenceId, result)

	// Clean up old duplicate entries for this client
	kv.cleanupDuplicateCache(cmd.ClientId)

	// Notify any waiting RPC handler
	kv.notifyWaiter(msg.CommandIndex, result)
}

// deserializeCommand unmarshals a protobuf Command into an Op
func (kv *KVServer) deserializeCommand(data []byte) (Op, error) {
	var pbCmd pb.Command
	if err := proto.Unmarshal(data, &pbCmd); err != nil {
		return Op{}, fmt.Errorf("failed to unmarshal command: %w", err)
	}

	var opType OpType
	switch pbCmd.Op {
	case pb.OpType_OP_TYPE_GET:
		opType = OpGet
	case pb.OpType_OP_TYPE_PUT:
		opType = OpPut
	case pb.OpType_OP_TYPE_APPEND:
		opType = OpAppend
	default:
		return Op{}, fmt.Errorf("unknown op type: %v", pbCmd.Op)
	}

	return Op{
		Type:       opType,
		Key:        pbCmd.Key,
		Value:      pbCmd.Value,
		ClientId:   pbCmd.ClientId,
		SequenceId: pbCmd.SequenceNum,
	}, nil
}

// applyCommand executes the operation on the state machine
func (kv *KVServer) applyCommand(cmd Op) Result {
	switch cmd.Type {
	case OpGet:
		value := kv.state[cmd.Key]
		return Result{Value: value, Err: nil}

	case OpPut:
		kv.state[cmd.Key] = cmd.Value
		return Result{Value: "", Err: nil}

	case OpAppend:
		kv.state[cmd.Key] += cmd.Value
		return Result{Value: "", Err: nil}

	default:
		return Result{Err: fmt.Errorf("unknown operation type: %v", cmd.Type)}
	}
}

// getDuplicate checks if an operation has already been processed
func (kv *KVServer) getDuplicate(clientId, seqNum int64) *DuplicateEntry {
	if clientMap, ok := kv.duplicates[clientId]; ok {
		if entry, ok := clientMap[seqNum]; ok {
			// Check if entry hasn't expired
			if time.Since(entry.Timestamp) < duplicateCacheExpiry {
				return entry
			}
		}
	}
	return nil
}

// saveDuplicate stores a completed operation result in the cache
func (kv *KVServer) saveDuplicate(clientId, seqNum int64, result Result) {
	if kv.duplicates[clientId] == nil {
		kv.duplicates[clientId] = make(map[int64]*DuplicateEntry)
	}
	kv.duplicates[clientId][seqNum] = &DuplicateEntry{
		Result:    result,
		Timestamp: time.Now(),
	}
}

// cleanupDuplicateCache removes old entries when cache exceeds limits
func (kv *KVServer) cleanupDuplicateCache(clientId int64) {
	clientMap := kv.duplicates[clientId]
	if clientMap == nil {
		return
	}

	// Remove expired entries
	now := time.Now()
	for seqNum, entry := range clientMap {
		if now.Sub(entry.Timestamp) > duplicateCacheExpiry {
			delete(clientMap, seqNum)
		}
	}

	// If still over limit, remove oldest entries (FIFO)
	for len(clientMap) > maxDuplicateEntriesPerClient {
		var oldestSeqNum int64
		var oldestTime time.Time
		first := true

		for seqNum, entry := range clientMap {
			if first || entry.Timestamp.Before(oldestTime) {
				oldestSeqNum = seqNum
				oldestTime = entry.Timestamp
				first = false
			}
		}

		delete(clientMap, oldestSeqNum)
	}

	// Clean up empty client maps
	if len(clientMap) == 0 {
		delete(kv.duplicates, clientId)
	}
}

// notifyWaiter sends the result to a waiting RPC handler
func (kv *KVServer) notifyWaiter(index int, result Result) {
	if pending, ok := kv.pendingOps[index]; ok {
		select {
		case pending.ResultCh <- result:
			// Successfully notified
		default:
			// Channel full or closed - client likely timed out
			// Result is already in duplicate cache, so retry will work
		}
		delete(kv.pendingOps, index)
	}
}

// drainApplyCh processes remaining messages on shutdown
func (kv *KVServer) drainApplyCh() {
	for {
		select {
		case msg := <-kv.applyCh:
			kv.processApplyMsg(msg)
		default:
			// No more messages
			return
		}
	}
}

// serializeCommand marshals an Op into protobuf bytes for Raft log
func (kv *KVServer) serializeCommand(op Op) ([]byte, error) {
	var pbOp pb.OpType
	switch op.Type {
	case OpGet:
		pbOp = pb.OpType_OP_TYPE_GET
	case OpPut:
		pbOp = pb.OpType_OP_TYPE_PUT
	case OpAppend:
		pbOp = pb.OpType_OP_TYPE_APPEND
	default:
		return nil, fmt.Errorf("unknown op type: %v", op.Type)
	}

	cmd := &pb.Command{
		Op:          pbOp,
		Key:         op.Key,
		Value:       op.Value,
		ClientId:    op.ClientId,
		SequenceNum: op.SequenceId,
	}

	return proto.Marshal(cmd)
}
