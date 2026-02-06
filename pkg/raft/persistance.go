package raft

// TODO: Implement persistence layer
//
// Required functionality:
// - Save(currentTerm, votedFor, log) to stable storage
// - Load() on startup to restore state
// - Must persist BEFORE responding to any RPC
// - Must use atomic writes (write to temp file, then rename)
//
// Files to manage:
// - raft-state.dat: currentTerm, votedFor, and metadata
// - raft-log.dat: log entries
//
// Key methods needed:
// - persist() - save all persistent state
// - readPersist() - restore state from disk
// - saveStateAndSnapshot() - for log compaction
//
// Raft requirement: State must survive crashes!
