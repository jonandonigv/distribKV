package kv

import (
	"testing"
	"time"

	"github.com/jonandonigv/distribKV/raft"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Serialization round-trip.
// ---------------------------------------------------------------------------

func TestSerializeDeserialize_RoundTrip(t *testing.T) {
	original := Op{
		Type:       OpPut,
		Key:        "foo",
		Value:      "bar",
		ClientId:   42,
		SequenceId: 7,
	}

	data, err := serializeCommand(original)
	require.NoError(t, err)

	recovered, err := deserializeCommand(data)
	require.NoError(t, err)
	assert.Equal(t, original.Type, recovered.Type)
	assert.Equal(t, original.Key, recovered.Key)
	assert.Equal(t, original.Value, recovered.Value)
	assert.Equal(t, original.ClientId, recovered.ClientId)
	assert.Equal(t, original.SequenceId, recovered.SequenceId)
}

func TestDeserializeCommand_CorruptBytes(t *testing.T) {
	_, err := deserializeCommand([]byte("not valid json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deserialize")
}

// ---------------------------------------------------------------------------
// Dedup cache + apply logic (whitebox, direct field manipulation).
// ---------------------------------------------------------------------------

// newTestServer builds a Server for unit tests WITHOUT starting the
// applyLoop goroutine (since the tests just call applyCommandLocked /
// dedup helpers directly). This avoids Kill()'s drain timeout.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	rf, err := raft.NewRaft(raft.Config{
		ServerID:            1,
		OwnAddr:             "127.0.0.1:0",
		Peers:               map[int]string{},
		ElectionTimeoutMin:  150 * time.Millisecond,
		ElectionTimeoutMax:  300 * time.Millisecond,
		HeartbeatInterval:   50 * time.Millisecond,
		Persister:           raft.NewMemoryPersister(),
	})
	require.NoError(t, err)
	return &Server{
		rf:         rf,
		applyCh:    rf.GetApplyCh(),
		state:      make(map[string]string),
		duplicates: make(map[int64]map[int64]*DuplicateEntry),
		pendingOps: make(map[int]*PendingOp),
		maxPending: DefaultMaxPendingOps,
		shutdownCh: make(chan struct{}),
	}
}

func TestApplyCommand_Put(t *testing.T) {
	s := newTestServer(t)


	s.mu.Lock()
	s.applyCommandLocked(Op{Type: OpPut, Key: "foo", Value: "bar"})
	val, ok := s.state["foo"]
	s.mu.Unlock()

	assert.True(t, ok)
	assert.Equal(t, "bar", val)
}

func TestApplyCommand_Append(t *testing.T) {
	s := newTestServer(t)


	s.mu.Lock()
	s.applyCommandLocked(Op{Type: OpPut, Key: "k", Value: "hello"})
	s.applyCommandLocked(Op{Type: OpAppend, Key: "k", Value: " world"})
	val := s.state["k"]
	s.mu.Unlock()

	assert.Equal(t, "hello world", val)
}

func TestApplyCommand_Get(t *testing.T) {
	s := newTestServer(t)


	s.mu.Lock()
	s.state["key"] = "value"
	result := s.applyCommandLocked(Op{Type: OpGet, Key: "key"})
	s.mu.Unlock()

	assert.Equal(t, "value", result.Value)
	assert.NoError(t, result.Err)
}

func TestApplyCommand_GetMissingKey(t *testing.T) {
	s := newTestServer(t)


	s.mu.Lock()
	result := s.applyCommandLocked(Op{Type: OpGet, Key: "nonexistent"})
	s.mu.Unlock()

	assert.ErrorIs(t, result.Err, ErrKeyNotFound)
}

func TestDedupCache_SameClientSeqReturnsCachedResult(t *testing.T) {
	s := newTestServer(t)


	clientId := int64(1)
	seqNum := int64(1)

	// First apply: Put succeeds, result is cached.
	s.mu.Lock()
	result := s.applyCommandLocked(Op{
		Type: OpPut, Key: "k", Value: "v",
		ClientId: clientId, SequenceId: seqNum,
	})
	s.saveDuplicateLocked(clientId, seqNum, result)
	s.mu.Unlock()
	require.NoError(t, result.Err)

	// Second call with same (clientId, seqNum): should be a dup.
	s.mu.Lock()
	dup := s.getDuplicateLocked(clientId, seqNum)
	s.mu.Unlock()
	require.NotNil(t, dup, "dedup entry should exist")
}

func TestDedupCache_ExpiredEntryEvicted(t *testing.T) {
	s := newTestServer(t)


	clientId := int64(2)
	seqNum := int64(1)

	// Insert a dedup entry with an old timestamp.
	s.mu.Lock()
	s.saveDuplicateLocked(clientId, seqNum, Result{})
	// Manually age the timestamp.
	s.duplicates[clientId][seqNum].Timestamp = time.Now().Add(-15 * time.Second)
	s.mu.Unlock()

	// Should be evicted on lookup.
	s.mu.Lock()
	dup := s.getDuplicateLocked(clientId, seqNum)
	s.mu.Unlock()
	assert.Nil(t, dup, "expired dedup entry should be evicted")
}

func TestDedupCache_CapEviction(t *testing.T) {
	s := newTestServer(t)


	clientId := int64(3)

	// Insert more than cap entries.
	s.mu.Lock()
	for i := 0; i < maxDuplicateEntriesPerClient+10; i++ {
		s.saveDuplicateLocked(clientId, int64(i), Result{})
		// Stagger timestamps so eviction order is deterministic.
		s.duplicates[clientId][int64(i)].Timestamp = time.Now().Add(time.Duration(i) * time.Microsecond)
	}
	// Cleanup should evict the oldest 10.
	s.cleanupDuplicateCacheLocked(clientId)
	count := len(s.duplicates[clientId])
	s.mu.Unlock()

	assert.LessOrEqual(t, count, maxDuplicateEntriesPerClient,
		"dedup cache should cap at %d entries, got %d", maxDuplicateEntriesPerClient, count)

	// The oldest entries (0..9) should be evicted; entries 10..110 should survive.
	s.mu.Lock()
	_, hasOld := s.duplicates[clientId][0]     // oldest — should be evicted
	_, hasNew := s.duplicates[clientId][109]   // newest — should survive
	s.mu.Unlock()
	assert.False(t, hasOld, "oldest entry should be evicted")
	assert.True(t, hasNew, "newest entry should survive")
}