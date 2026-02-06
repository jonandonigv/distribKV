package snapshot

// TODO: Implement log compaction via snapshotting
//
// Purpose: Reduce log size and speed up recovery for slow followers
//
// Key Components:
//
// 1. Snapshot Structure:
//    - lastIncludedIndex: last log entry in snapshot
//    - lastIncludedTerm: term of last log entry
//    - stateMachineState: serialized state (key-value pairs)
//    - Should include Raft metadata for consistency
//
// 2. Snapshot Installation:
//    - Leader sends InstallSnapshot RPC to follower
//    - Follower replaces log with snapshot
//    - Follower resets state machine from snapshot
//
// 3. Truncation:
//    - Delete log entries before snapshot
//    - Keep recent entries for efficiency
//    - Save snapshot separately from log
//
// 4. RPC: InstallSnapshot
//    - Split large snapshots into chunks
//    - Stream chunks to follower
//    - Handle partial failures
//
// Required Methods:
// - CreateSnapshot(lastIndex int) []byte
// - InstallSnapshot(data []byte) error
// - SendSnapshot(peerId int, snapshot []byte) error
// - CondInstallSnapshot(lastTerm, lastIndex int, snapshot []byte) bool
//
// Integration with KV Server:
// - KV server serializes its state
// - Raft saves snapshot and truncates log
// - On restart: load snapshot, restore state machine, replay remaining log
