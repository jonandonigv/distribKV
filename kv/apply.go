// Apply loop: reads committed entries from raft's applyCh, deserializes
// the Command, executes on the local state map, and notifies the waiting
// RPC handler via the pendingOps channel. Duplicate detection via
// clientId/seqNum cache (cap 100/client, TTL 10s).
//
// See AGENTS.md "KV Service Notes".

package kv

import (
	"fmt"
	"time"

	"github.com/jonandonigv/distribKV/raft"
)

// applyLoop runs in its own goroutine (started by NewServer). It reads
// raft.ApplyMsg from applyCh, deserializes the embedded Command, applies
// it to the KV state machine, and notifies the RPC handler waiting on
// the entry's index. It exits when applyCh is closed (by Kill's drain
// goroutine) or when it sees a shutdown signal.
func (s *Server) applyLoop() {
	for {
		msg, ok := <-s.applyCh
		if !ok {
			// applyCh closed by Kill's drain goroutine — exit cleanly.
			return
		}
		s.processApplyMsg(msg)
	}
}

// processApplyMsg is the single dispatcher for messages from raft's
// applyCh. Two message kinds:
//   - SnapshotValid: rehydrate {state, dedup} wholesale (kv/snapshot.go).
//   - CommandValid: deserialize the Command, apply it to the state map,
//     notify the waiting RPC handler. Handles:
//   - Deserialize failure: notify waiter with error, don't apply
//     (closes 0.0.x TODO #28).
//   - Duplicate detection: (clientId, seqNum) already applied → cached
//     result without re-executing.
//   - Normal apply: Put overwrites, Append concatenates, Get reads.
func (s *Server) processApplyMsg(msg raft.ApplyMsg) {
	if msg.SnapshotValid {
		s.handleSnapshotMsg(msg)
		return
	}
	if !msg.CommandValid {
		return
	}

	op, err := deserializeCommand(msg.Command)
	if err != nil {
		s.logger.Error("deserialize failed",
			"index", msg.CommandIndex, "err", err)
		s.notifyWaiter(msg.CommandIndex, Result{Err: err})
		return
	}

	s.mu.Lock()

	// Check dedup cache. If we've already applied this (clientId, seqNum),
	// return the cached result without re-executing. Skip when both are
	// 0 (no-dedup sentinel for 0.1.0).
	if op.ClientId != 0 && op.SequenceId != 0 {
		if dup := s.getDuplicateLocked(op.ClientId, op.SequenceId); dup != nil {
			s.mu.Unlock()
			s.notifyWaiter(msg.CommandIndex, dup.Result)
			return
		}
	}

	// Apply the command to the state machine.
	result := s.applyCommandLocked(op)
	s.lastApplied = msg.CommandIndex

	// Cache the result for duplicate detection.
	s.saveDuplicateLocked(op.ClientId, op.SequenceId, result)

	// Clean up expired entries for this client.
	s.cleanupDuplicateCacheLocked(op.ClientId)

	s.mu.Unlock()

	// Notify the waiting RPC handler.
	s.notifyWaiter(msg.CommandIndex, result)

	// Log-compaction trigger (PLAN.md S1d): fold applied territory into
	// raft when enough entries accumulated since the last snapshot.
	s.takeSnapshot()
}

// applyCommandLocked executes an Op on the KV state map. Caller must
// hold s.mu.
func (s *Server) applyCommandLocked(op Op) Result {
	switch op.Type {
	case OpPut:
		s.state[op.Key] = op.Value
		return Result{}
	case OpAppend:
		s.state[op.Key] = s.state[op.Key] + op.Value
		return Result{}
	case OpGet:
		val, ok := s.state[op.Key]
		if !ok {
			return Result{Err: ErrKeyNotFound}
		}
		return Result{Value: val}
	default:
		return Result{Err: fmt.Errorf("unknown op type: %v", op.Type)}
	}
}

// notifyWaiter sends a Result to the RPC handler waiting on the given
// log index. If no waiter exists yet (the RPC handler hasn't registered
// pendingOps[index] because ReplicateCommand is still returning), the
// result is stashed in s.recent so submitOperation can pick it up once it
// registers — see submitOperation. If the handler already timed out and
// removed its entry, the result is dropped; the dedup cache retains it so
// a client retry will still get the right answer.
func (s *Server) notifyWaiter(index int, result Result) {
	s.mu.Lock()
	pending, ok := s.pendingOps[index]
	if ok {
		delete(s.pendingOps, index)
		s.mu.Unlock()

		// Non-blocking send: if the channel is full (client timed out
		// and isn't reading), drop the result. Dedup cache has it.
		select {
		case pending.ResultCh <- result:
		default:
		}
		return
	}

	// No waiter yet — stash for the late-registering RPC handler.
	s.recent[index] = result
	// Bound the stash; evict oldest entries when full.
	for len(s.recent) > s.maxPending {
		for idx := range s.recent {
			delete(s.recent, idx)
			break
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Duplicate detection cache.
// ---------------------------------------------------------------------------

// getDuplicateLocked returns the cached result for (clientId, seqNum),
// or nil if not found / expired. Caller must hold s.mu.
func (s *Server) getDuplicateLocked(clientId, seqNum int64) *DuplicateEntry {
	clientMap, ok := s.duplicates[clientId]
	if !ok {
		return nil
	}
	entry, ok := clientMap[seqNum]
	if !ok {
		return nil
	}
	// Check TTL.
	if time.Since(entry.Timestamp) > duplicateCacheExpiry {
		delete(clientMap, seqNum)
		return nil
	}
	return entry
}

// saveDuplicateLocked caches the result for (clientId, seqNum). Caller
// must hold s.mu.
func (s *Server) saveDuplicateLocked(clientId, seqNum int64, result Result) {
	clientMap, ok := s.duplicates[clientId]
	if !ok {
		clientMap = make(map[int64]*DuplicateEntry)
		s.duplicates[clientId] = clientMap
	}
	clientMap[seqNum] = &DuplicateEntry{
		Result:    result,
		Timestamp: time.Now(),
	}
}

// cleanupDuplicateCacheLocked removes expired entries for the given
// client. If the per-client cache exceeds maxDuplicateEntriesPerClient,
// the oldest entries are evicted. Caller must hold s.mu.
func (s *Server) cleanupDuplicateCacheLocked(clientId int64) {
	clientMap, ok := s.duplicates[clientId]
	if !ok {
		return
	}

	// Remove expired entries.
	for seq, entry := range clientMap {
		if time.Since(entry.Timestamp) > duplicateCacheExpiry {
			delete(clientMap, seq)
		}
	}

	// If still over the cap, remove the oldest entries.
	if len(clientMap) > maxDuplicateEntriesPerClient {
		// Find and remove the oldest entries until we're under the cap.
		// This is O(n) in the number of entries, but maxDuplicateEntriesPerClient
		// is 100 so it's cheap.
		for len(clientMap) > maxDuplicateEntriesPerClient {
			var oldestSeq int64
			var oldestTime time.Time
			first := true
			for seq, entry := range clientMap {
				if first || entry.Timestamp.Before(oldestTime) {
					oldestSeq = seq
					oldestTime = entry.Timestamp
					first = false
				}
			}
			delete(clientMap, oldestSeq)
		}
	}
}
