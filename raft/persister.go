// Persister abstracts durable Raft state so tests can inject in-memory or
// fault-injecting variants without touching disk. See AGENTS.md "Identity
// & Persistence".
//
// For 0.1.0 the interface is just Save/Load. Snapshot methods will be
// added when snapshotting is implemented; they will be additive and
// non-breaking.

package raft

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Persister is the seam between Raft and durable storage.
type Persister interface {
	// Save persists currentTerm, votedFor, and the full log. Must be
	// called before responding to any RPC (Raft's persist-before-respond
	// invariant). Implementations must be goroutine-safe.
	Save(currentTerm int, votedFor int, log []LogEntry) error

	// Load restores state previously written by Save. When no persisted
	// state exists, implementations return an error the caller can
	// detect; a fresh node then starts with currentTerm=0, votedFor=-1.
	Load() (currentTerm int, votedFor int, log []LogEntry, err error)
}

// ---------------------------------------------------------------------------
// MemoryPersister — in-memory implementation used by tests.
// ---------------------------------------------------------------------------

// MemoryPersister stores Raft state in process memory. It is goroutine-safe
// and can simulate disk failures via SetSaveErr.
type MemoryPersister struct {
	mu          sync.Mutex
	currentTerm int
	votedFor    int
	log         []LogEntry
	saveErr     error
}

// NewMemoryPersister returns a ready in-memory persister. Initial state
// mirrors a fresh node: currentTerm=0, votedFor=-1, empty log.
func NewMemoryPersister() *MemoryPersister {
	return &MemoryPersister{
		votedFor: -1,
		log:      nil,
	}
}

// SetSaveErr injects an error to be returned by the next Save call. Pass
// nil to clear. Used by fault-injecting tests (e.g. persist-before-respond
// regression tests).
func (m *MemoryPersister) SetSaveErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveErr = err
}

// Save stores the state in memory.
func (m *MemoryPersister) Save(currentTerm int, votedFor int, log []LogEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.currentTerm = currentTerm
	m.votedFor = votedFor
	// Copy the log so later mutation by the caller doesn't affect stored
	// state (Save is a snapshot, not a reference).
	m.log = make([]LogEntry, len(log))
	for i, e := range log {
		// Also copy the Command slice; LogEntry.Command is mutable.
		cmd := make([]byte, len(e.Command))
		copy(cmd, e.Command)
		m.log[i] = LogEntry{Index: e.Index, Term: e.Term, Command: cmd}
	}
	return nil
}

// Load returns the in-memory state. Returns a sentinel "no state" error
// when nothing has been saved yet, mirroring FilePersister's missing-file
// behavior.
func (m *MemoryPersister) Load() (int, int, []LogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.log == nil && m.currentTerm == 0 && m.votedFor == -1 {
		return 0, -1, nil, fmt.Errorf("persisted state does not exist")
	}
	// Return copies so callers can't mutate our internal state.
	log := make([]LogEntry, len(m.log))
	for i, e := range m.log {
		cmd := make([]byte, len(e.Command))
		copy(cmd, e.Command)
		log[i] = LogEntry{Index: e.Index, Term: e.Term, Command: cmd}
	}
	return m.currentTerm, m.votedFor, log, nil
}

// ---------------------------------------------------------------------------
// FilePersister — JSON-on-disk implementation used in production.
// ---------------------------------------------------------------------------

// persistentState is the on-disk JSON shape for raft-state.json.
type persistentState struct {
	CurrentTerm int               `json:"currentTerm"`
	VotedFor    int               `json:"votedFor"`
	Log         []persistentEntry `json:"log"`
}

// persistentEntry is one LogEntry in the durable format; Command is
// base64-encoded so arbitrary bytes round-trip through JSON cleanly.
type persistentEntry struct {
	Index   int64  `json:"index"`
	Term    int64  `json:"term"`
	Command string `json:"command"` // base64 of LogEntry.Command
}

// FilePersister persists Raft state to a single JSON file at filename.
// Writes are atomic: data goes to <filename>.tmp first, is fsync'd, then
// renamed into place. Concurrent Saves are serialized by a mutex.
type FilePersister struct {
	mu       sync.Mutex
	filename string
}

// NewFilePersister returns a persister that reads and writes filename.
// The file is created on the first Save; Load returns a "does not exist"
// error if the file is absent (fresh node).
func NewFilePersister(filename string) *FilePersister {
	return &FilePersister{filename: filename}
}

// Save atomically writes currentTerm, votedFor, and log to disk.
func (p *FilePersister) Save(currentTerm int, votedFor int, log []LogEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	persistentLog := make([]persistentEntry, len(log))
	for i, e := range log {
		persistentLog[i] = persistentEntry{
			Index:   e.Index,
			Term:    e.Term,
			Command: base64.StdEncoding.EncodeToString(e.Command),
		}
	}

	state := persistentState{
		CurrentTerm: currentTerm,
		VotedFor:    votedFor,
		Log:         persistentLog,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return p.atomicWrite(data)
}

// Load reads previously-saved state. Returns an error containing "does not
// exist" if the file is missing.
func (p *FilePersister) Load() (int, int, []LogEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := os.Stat(p.filename); os.IsNotExist(err) {
		return 0, -1, nil, fmt.Errorf("persisted state file does not exist: %w", err)
	}

	data, err := os.ReadFile(p.filename)
	if err != nil {
		return 0, -1, nil, fmt.Errorf("read persisted state: %w", err)
	}

	var state persistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, -1, nil, fmt.Errorf("decode persisted state: %w", err)
	}

	log := make([]LogEntry, len(state.Log))
	for i, e := range state.Log {
		cmd, err := base64.StdEncoding.DecodeString(e.Command)
		if err != nil {
			return 0, -1, nil, fmt.Errorf("decode log entry %d command: %w", i, err)
		}
		log[i] = LogEntry{Index: e.Index, Term: e.Term, Command: cmd}
	}
	return state.CurrentTerm, state.VotedFor, log, nil
}

// atomicWrite writes data to <filename>.tmp, fsyncs, then renames into
// place. On failure no .tmp file is left behind and the target file (if
// any) is untouched.
func (p *FilePersister) atomicWrite(data []byte) error {
	tempFile := p.filename + ".tmp"

	// Ensure the parent directory exists. Create-it-once is cheap; if
	// the parent path is actually a file, MkdirAll returns an error and
	// we bail out before writing anything to .tmp.
	if err := os.MkdirAll(filepath.Dir(p.filename), 0755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}

	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	f, err := os.Open(tempFile)
	if err != nil {
		_ = os.Remove(tempFile)
		return fmt.Errorf("open temp file for fsync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tempFile)
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tempFile)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tempFile, p.filename); err != nil {
		_ = os.Remove(tempFile)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
