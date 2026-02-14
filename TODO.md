# TODO: Bugs, Errors, and Issues

## Deadlocks (Critical - Primary Cause of Deadlocks)

### 1. Blocking Send to applyCh While Holding Mutex
- **File**: `pkg/raft/raft.go:347-381`
- **Severity**: CRITICAL
- **Issue**: `applyCommittedEntries` goroutine sends to `applyCh` (buffer 10) without holding the mutex, but the KVServer's `applyLoop` can get blocked if it holds its own lock while processing. If the channel buffer fills up, this causes deadlock.
- **Fix**: Increase `applyCh` buffer size from 10 to 100+, or use non-blocking send with select.

### 2. AppendEntries Holds Lock During Persist
- **File**: `pkg/raft/replication.go:26-158`
- **Severity**: HIGH
- **Issue**: The `AppendEntries` RPC handler holds `r.mu` while calling `r.persist()` at line 153. This is called every heartbeat (~50ms), causing lock contention.
- **Fix**: Release `r.mu` before calling `persist()`, or make persist non-blocking.

### 3. RequestVote Holds Lock During Persist
- **File**: `pkg/raft/election.go:232-289`
- **Severity**: MEDIUM-HIGH
- **Issue**: The `RequestVote` RPC handler holds `r.mu` while calling `r.persist()` at line 285.
- **Fix**: Same as #2.

### 4. Goroutines Spawned While Holding Lock
- **File**: `pkg/raft/replication.go:197-199`
- **Severity**: MEDIUM
- **Issue**: `ReplicateCommand` spawns goroutines to send AppendEntries while still holding `r.mu`.
- **Fix**: Move goroutine spawns after releasing lock.

### 5. Recursive Goroutine Spawn While Holding Lock
- **File**: `pkg/raft/replication.go:322`
- **Severity**: LOW-MEDIUM
- **Issue**: When log mismatch occurs, `sendAppendEntries` recursively spawns another goroutine while holding `r.mu`.
- **Fix**: Use non-recursive retry with proper lock release.

### 6. Double Lock Pattern in sendRequestVote
- **File**: `pkg/raft/election.go:146-163`
- **Severity**: LOW
- **Issue**: Inconsistent locking - acquires `r.mu`, releases, then acquires again.
- **Fix**: Refactor to use single lock acquisition.

---

## Logic Errors

### 7. Leader ID Tracking Issue
- **File**: `pkg/raft/replication.go:51-54`
- **Issue**: `leaderId` is set on every AppendEntries, even for followers. Should only be set when transitioning to follower or on valid leader contact.
- **Fix**: Only set `leaderId` when `r.state == Follower` or when first discovering a leader.

### 8. commitIndex Not Signalized on Leader Commit
- **File**: `pkg/raft/replication.go:304-314`
- **Issue**: After leader commits entries, `applyCond.Broadcast()` is called in `updateCommitIndex()`, but there's a race - the commit might happen before the apply goroutine is waiting.
- **Fix**: Ensure apply goroutine is woken up properly; verify `applyCond.Wait()` is correctly signaling.

### 9. No Validation of LeaderCommit Against Log
- **File**: `pkg/raft/replication.go:123-150`
- **Issue**: When leader sends `leaderCommit`, the follower trusts it entirely. If there's a bug in leader or log truncation happened, could commit invalid entries.
- **Fix**: Validate `leaderCommit` against actual log size before advancing commitIndex.

### 10. Election Timer Not Reset on Leader Step Down
- **File**: `pkg/raft/election.go:169-179`
- **Issue**: `stepDown` calls `resetElectionTimer()` but if node becomes candidate again immediately, timer might not have properly reset.
- **Fix**: Ensure timer is fully reset with proper channel drain.

### 11. Vote Counting Race Condition
- **File**: `pkg/raft/election.go:146-163`
- **Issue**: Uses separate `votesMutex` but reads `r.state` without holding `r.mu` after releasing it, creating a race condition.
- **Fix**: Hold `r.mu` for entire vote counting and leader election check.

---

## Error Handling Issues

### 12. Persist Error Not Handled Properly in AppendEntries
- **File**: `pkg/raft/replication.go:153-155`
- **Issue**: If `persist()` fails, returns error but may have already modified in-memory state. Could cause inconsistency.
- **Fix**: Either rollback in-memory changes on persist failure, or ensure persist succeeds before modifying state.

### 13. Persist Error Not Handled Properly in RequestVote
- **File**: `pkg/raft/election.go:285-287`
- **Issue**: Same as #12.

### 14. No Error Handling for Failed goroutines in ReplicateCommand
- **File**: `pkg/raft/replication.go:197-199`
- **Issue**: If `go r.sendAppendEntries(peerId)` fails to start (unlikely but possible), there's no error handling.
- **Fix**: Not critical but could log failures.

---

## Resource Leaks and Cleanup Issues

### 15. Election Timer Not Stopped on Shutdown
- **File**: `pkg/raft/raft.go`
- **Issue**: No `Shutdown()` or `Close()` method to stop the election timer goroutine.
- **Fix**: Add a shutdown method that closes `electionStopChan`.

### 16. Heartbeat goroutine Not Cleaned Up
- **File**: `pkg/raft/election.go:294-308`
- **Issue**: `startHeartbeat` creates goroutine but there's no explicit cleanup on leader step down besides closing the channel.
- **Fix**: Ensure all goroutines properly terminate.

### 17. Persist File Handle Not Closed on Error
- **File**: `pkg/raft/persistance.go:142`
- **Issue**: In `atomicWrite`, if `f.Sync()` fails after `f.Close()` succeeds (unlikely), there's potential handle leak.
- **Fix**: Use defer more carefully, handle error path better.

---

## Missing Functionality

### 18. No Snapshotting Support
- **Issue**: Log grows unbounded. Need snapshotting per Raft paper section 7.
- **Fix**: Implement `Snapshot()` method and log compaction.

### 19. No Leadership Transfer
- **Issue**: Leader can't transfer leadership to another node for load balancing.
- **Fix**: Implement leadership transfer (not in original Raft but useful).

### 20. No Pre-Vote Algorithm
- **Issue**: Can cause disruption during network partitions.
- **Fix**: Implement pre-vote per Raft paper section 9.5.

---

## Testing Issues

### 21. Tests Use time.Sleep for Synchronization
- **Files**: `pkg/raft/raft_test.go`, `pkg/kvserver/server_test.go`
- **Issue**: Tests rely on sleeps which can be flaky.
- **Fix**: Use proper synchronization primitives (channels, wait groups).

### 22. No Network Partition Testing
- **Issue**: No tests for split-brain scenarios.
- **Fix**: Add tests that simulate network partitions.

---

## Minor Issues

### 23. Logging Too Verbose in Hot Path
- **Files**: `pkg/raft/replication.go`, `pkg/raft/election.go`
- **Issue**: Log statements in `sendAppendEntries`, `AppendEntries` handler create noise.
- **Fix**: Reduce logging in hot paths, use debug logging.

### 24. Inconsistent Error Messages
- **Issue**: Error messages don't follow consistent format.
- **Fix**: Standardize error message format.

### 25. No Metrics/Observability
- **Issue**: No Prometheus metrics, tracing, or health endpoints.
- **Fix**: Add metrics for commitIndex, term, state, etc.

---

## Summary

| Category | Count |
|----------|-------|
| Deadlocks | 6 |
| Logic Errors | 5 |
| Error Handling | 3 |
| Resource Leaks | 3 |
| Missing Functionality | 3 |
| Testing Issues | 2 |
| Minor Issues | 3 |

**Total: 25 issues**
