package raft

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// persistentState represents the state that must survive crashes
type persistentState struct {
	CurrentTerm int               `json:"currentTerm"`
	VotedFor    int               `json:"votedFor"`
	Log         []persistentEntry `json:"log"`
}

// persistentEntry represents a log entry in JSON format
type persistentEntry struct {
	Index   int64  `json:"index"`
	Term    int    `json:"term"`
	Command string `json:"command"` // base64 encoded
}

// Persister handles saving and loading Raft state to/from disk.
// It ensures atomic writes and fsync for durability.
type Persister struct {
	mu       sync.Mutex
	filename string
}

// NewPersister creates a new Persister for the given filename.
func NewPersister(filename string) *Persister {
	return &Persister{
		filename: filename,
	}
}

// Save persists the current state to disk atomically.
// It writes to a temp file, fsyncs, then atomically renames.
// Returns error if persist fails (caller should not respond to RPC).
func (p *Persister) Save(currentTerm int, votedFor int, log []LogEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Convert log entries to persistent format with base64 encoding
	persistentLog := make([]persistentEntry, len(log))
	for i, entry := range log {
		persistentLog[i] = persistentEntry{
			Index:   entry.Index,
			Term:    entry.Term,
			Command: base64.StdEncoding.EncodeToString(entry.Command),
		}
	}

	state := persistentState{
		CurrentTerm: currentTerm,
		VotedFor:    votedFor,
		Log:         persistentLog,
	}

	// Encode to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode state to JSON: %w", err)
	}

	// Perform atomic write
	if err := p.atomicWrite(data); err != nil {
		return fmt.Errorf("failed to persist state: %w", err)
	}

	return nil
}

// Load restores state from disk.
// Returns error if file doesn't exist or is corrupted.
func (p *Persister) Load() (currentTerm int, votedFor int, log []LogEntry, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if file exists
	if _, err := os.Stat(p.filename); os.IsNotExist(err) {
		return 0, -1, nil, fmt.Errorf("persisted state file does not exist: %w", err)
	}

	// Read file
	data, err := os.ReadFile(p.filename)
	if err != nil {
		return 0, -1, nil, fmt.Errorf("failed to read persisted state file: %w", err)
	}

	// Decode JSON
	var state persistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, -1, nil, fmt.Errorf("failed to decode persisted state: %w", err)
	}

	// Convert persistent log entries back to LogEntry with base64 decoding
	log = make([]LogEntry, len(state.Log))
	for i, entry := range state.Log {
		command, err := base64.StdEncoding.DecodeString(entry.Command)
		if err != nil {
			return 0, -1, nil, fmt.Errorf("failed to decode log entry command at index %d: %w", i, err)
		}
		log[i] = LogEntry{
			Index:   entry.Index,
			Term:    entry.Term,
			Command: command,
		}
	}

	return state.CurrentTerm, state.VotedFor, log, nil
}

// atomicWrite performs an atomic file write with fsync for durability.
// 1. Write to temp file
// 2. fsync to ensure data is on disk
// 3. Atomic rename to final filename
func (p *Persister) atomicWrite(data []byte) error {
	// Create temp file in same directory for atomic rename
	dir := filepath.Dir(p.filename)
	tempFile := p.filename + ".tmp"

	// Ensure directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write to temp file
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Open file for fsync
	f, err := os.Open(tempFile)
	if err != nil {
		os.Remove(tempFile) // Clean up on error
		return fmt.Errorf("failed to open temp file for fsync: %w", err)
	}
	defer f.Close()

	// Fsync to ensure data is written to disk
	if err := f.Sync(); err != nil {
		os.Remove(tempFile) // Clean up on error
		return fmt.Errorf("failed to fsync temp file: %w", err)
	}

	// Close file before rename
	if err := f.Close(); err != nil {
		os.Remove(tempFile) // Clean up on error
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempFile, p.filename); err != nil {
		os.Remove(tempFile) // Clean up on error
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}
