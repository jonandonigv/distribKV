// KV-side snapshot support (PLAN.md Step S1d): serialization of the
// {state, dedup} payload, rehydration from SnapshotValid ApplyMsgs, and
// the applied-entry-count trigger that calls rf.Snapshot.
//
// The dedup cache rides inside the snapshot so a restarted node honors
// client retries whose entries were already compacted away — closing the
// restart-replay gap documented in AGENTS.md "KV Service Notes".

package kv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jonandonigv/distribKV/raft"
)

// snapshotEntry is the JSON shape of one cached dedup result. Result.Err
// does not round-trip through JSON, so errors are stored as strings and
// known sentinels are restored on load (a retried Get keeps its
// key-not-found semantics across a snapshot).
type snapshotEntry struct {
	Value     string    `json:"value"`
	Err       string    `json:"err,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// snapshotPayload is the full state-machine image handed to rf.Snapshot.
type snapshotPayload struct {
	State map[string]string                  `json:"state"`
	Dedup map[int64]map[int64]*snapshotEntry `json:"dedup,omitempty"`
}

// serializeSnapshot encodes the current KV state + non-expired dedup
// entries. Expired entries are dropped here rather than resurrected into
// a fresh incarnation.
func serializeSnapshot(state map[string]string, dedup map[int64]map[int64]*DuplicateEntry) ([]byte, error) {
	payload := snapshotPayload{
		State: state,
		Dedup: make(map[int64]map[int64]*snapshotEntry),
	}
	for clientId, clientMap := range dedup {
		for seqNum, entry := range clientMap {
			// Skip the (0,0) no-dedup sentinel (same rule as the lookup
			// path) — it carries no retry semantics worth persisting.
			if clientId == 0 && seqNum == 0 {
				continue
			}
			if time.Since(entry.Timestamp) > duplicateCacheExpiry {
				continue // expired; the TTL would kill it anyway
			}
			if payload.Dedup[clientId] == nil {
				payload.Dedup[clientId] = make(map[int64]*snapshotEntry)
			}
			payload.Dedup[clientId][seqNum] = &snapshotEntry{
				Value:     entry.Result.Value,
				Err:       errString(entry.Result.Err),
				Timestamp: entry.Timestamp,
			}
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("serialize snapshot: %w", err)
	}
	return data, nil
}

// deserializeSnapshot decodes a snapshot payload back into live maps.
func deserializeSnapshot(data []byte) (map[string]string, map[int64]map[int64]*DuplicateEntry, error) {
	var payload snapshotPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, fmt.Errorf("deserialize snapshot: %w", err)
	}

	state := payload.State
	if state == nil {
		state = make(map[string]string)
	}
	dedup := make(map[int64]map[int64]*DuplicateEntry, len(payload.Dedup))
	for clientId, clientMap := range payload.Dedup {
		dedup[clientId] = make(map[int64]*DuplicateEntry, len(clientMap))
		for seqNum, e := range clientMap {
			dedup[clientId][seqNum] = &DuplicateEntry{
				Result:    Result{Value: e.Value, Err: sentinelErr(e.Err)},
				Timestamp: e.Timestamp,
			}
		}
	}
	return state, dedup, nil
}

// errString renders an error for the snapshot JSON.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sentinelErr restores known error sentinels from their string form.
func sentinelErr(s string) error {
	if s == "" {
		return nil
	}
	if s == ErrKeyNotFound.Error() {
		return ErrKeyNotFound
	}
	return nil // unknown error kinds degrade to "no error" post-restart
}

// handleSnapshotMsg applies a SnapshotValid ApplyMsg: wholesale replace
// of state + dedup, bookkeeping jump to LastIncludedIndex, recent cleared.
// No waiter is notified — no in-flight RPC corresponds to a snapshot.
func (s *Server) handleSnapshotMsg(msg raft.ApplyMsg) {
	state, dedup, err := deserializeSnapshot(msg.SnapshotData)
	if err != nil {
		// Keep the prior state rather than corrupting it with garbage;
		// log loudly since this means an unreadable snapshot shipped
		// through Raft.
		s.logger.Error("snapshot deserialize failed",
			slog.Int("index", msg.LastIncludedIndex), slog.String("err", err.Error()))
		return
	}

	s.mu.Lock()
	s.state = state
	s.duplicates = dedup
	s.lastApplied = msg.LastIncludedIndex
	s.lastSnapshotIndex = msg.LastIncludedIndex
	s.recent = make(map[int]Result)
	s.mu.Unlock()

	s.logger.Info("kv state restored from snapshot",
		slog.Int("index", msg.LastIncludedIndex),
		slog.Int("term", msg.LastIncludedTerm),
		slog.Int("keys", len(state)))
}

// takeSnapshot serializes the current state and hands it to raft. Called
// from the apply loop after each applied command. Failures are logged and
// non-fatal: snapshotting is an optimization — lastSnapshotIndex only
// advances after raft confirms success, so the trigger simply retriggers
// on a later apply.
func (s *Server) takeSnapshot() {
	threshold := s.rf.SnapshotThreshold()
	if threshold <= 0 {
		return
	}

	s.mu.Lock()
	if s.lastApplied-s.lastSnapshotIndex < threshold {
		s.mu.Unlock()
		return
	}
	idx := s.lastApplied
	data, err := serializeSnapshot(s.state, s.duplicates)
	s.mu.Unlock()
	if err != nil {
		s.logger.Error("serialize snapshot failed",
			slog.Int("index", idx), slog.String("err", err.Error()))
		return
	}

	if err := s.rf.Snapshot(idx, data); err != nil {
		s.logger.Warn("raft snapshot rejected",
			slog.Int("index", idx), slog.String("err", err.Error()))
		return
	}

	s.mu.Lock()
	s.lastSnapshotIndex = idx
	s.mu.Unlock()

	s.logger.Info("kv snapshot taken", slog.Int("index", idx), slog.Int("bytes", len(data)))
}
