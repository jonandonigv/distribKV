package raft

// TestHarness provides infrastructure for testing Raft consensus.
// This file contains shared testing utilities, mock implementations,
// and helper functions to enable comprehensive Raft testing.

// TODO: Implement test harness components:
// - MockNetwork: Simulated network with partition/drop/delay control
// - MockPersister: In-memory storage for fast, deterministic persistence
// - TestCluster: Multi-node cluster management
// - Synchronization helpers for distributed conditions
// - Deterministic timing control
