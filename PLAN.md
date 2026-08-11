# Snapshotting plan (0.2.0)

Log compaction (snapshotting) for distribKV. The 0.1.x seams are already
baked in (verified in the codebase): `ApplyMsg` carries snapshot fields
(`raft/types.go:51-54`), `logBase` arithmetic is used in every log access
(`sendAppendEntries`, `applyPendingEntries`), `InstallSnapshot` RPC is
declared with an `Unimplemented` handler (`raft/replication.go:410`),
`snapshot_threshold` config knob is parsed, and the KV applyLoop
`continue`s on `!CommandValid` (`kv/apply.go:29`).

**Branch:** `0.2.0` off `main`.
**Trigger:** applied-entry count (`lastApplied - lastSnapshotIndex >= threshold`).
**Snapshot payload:** `{state map, non-expired dedup entries}` — closes the
  documented restart-replay gap (AGENTS.md KV Notes: "leader restart could
  re-execute a client retry"). Dedup is capped at 100/client, cheap.
**InstallSnapshot:** single chunk, `done=true`, ~4MB cap.
**PRs:** two — S1 then S2 (`0.2.0` → `dev` → `main` per step).

---

## Step S1 — local snapshot + restart recovery (no RPC)

### Persister interface (additive, non-breaking)

Add to `raft.Persister`:
- `SaveSnapshot(data []byte, lastIncludedIndex, lastIncludedTerm int) error`
- `LoadSnapshot() (data []byte, lastIncludedIndex, lastIncludedTerm int, err error)`

Implement on both in-repo implementors (no external implementors exist):
- `MemoryPersister`: in-memory fields.
- `FilePersister`: `<dir>/snapshot.bin` — header
  `{last_included_index, last_included_term}` + opaque bytes, atomic write
  (temp + fsync + rename, mirroring `atomicWrite`).

### Raft in-memory snapshot state

Add fields to `Raft`:
- `r.snapshot []byte`
- `r.snapshotIndex int`
- `r.snapshotTerm int`

Populated by `Snapshot()` and by `loadPersistedState()` on recovery.

### `Raft.Snapshot(index int, data []byte) error`

Called by KV after applying entries. Under `r.mu`:
- `index > commitIndex` → error (don't snapshot uncommitted).
- `index <= logBase` → no-op (already snapshotted past here).
- Snapshot the term of the entry at `index` (the about-to-be-truncated
  last entry): record `snapshotTerm = log[index - logBase - 1].Term`.
- Truncate `r.log` to entries with `Index > index` (keep the tail).
- Bump `r.logBase = index`.
- Set `r.snapshotIndex = index`, `r.snapshotTerm` (from above),
  `r.snapshot = data`.
- Persist: `persister.Save(...)` (raft-state) AND `persister.SaveSnapshot(...)`.

### Recovery in `loadPersistedState`

Extend the existing `loadPersistedState` (`raft/raft.go:241`):
1. `persister.LoadSnapshot()` → if present, set `r.logBase`,
   `r.snapshotIndex`, `r.snapshotTerm`, `r.snapshot`, and send an
   `ApplyMsg{SnapshotValid: true, SnapshotData: data,
   LastIncludedIndex: idx, LastIncludedTerm: term}` into `r.applyCh`
   (buffered 100; KV's applyLoop reads it during `NewServer`).
2. `persister.Load()` → restore `currentTerm`, `votedFor`, `log` as today.
3. Set `r.commitIndex = r.lastApplied = r.snapshotIndex` (the floor —
   no replay of snapshot-territory entries). When no snapshot, keep the
   current behavior (log-based floor).

This **closes the `TestStart_RestartRecoversRaftAndAcceptsNewWrites` gap**
documented in `server/server_test.go` — the KV map rehydrates from the
snapshot instead of being empty after restart.

### KV applyLoop snapshot handling

In `kv/apply.go` applyLoop, handle `msg.SnapshotValid`:
- Deserialize `SnapshotData` into `{state map[string]string, dedup
  map[int64]map[int64]*DuplicateEntry}` (only non-expired entries).
- Replace `s.state` wholesale with the snapshot's state.
- Replace `s.duplicates` with the snapshot's dedup (non-expired only).
- Set `s.lastApplied = msg.LastIncludedIndex`.
- Set `s.lastSnapshotIndex = msg.LastIncludedIndex`.
- Clear `s.recent` (history is replaced by the snapshot).
- Do NOT notify any waiter (no in-flight RPC corresponds to a snapshot).

Add fields to `kv.Server`:
- `lastApplied int`    // highest applied index (command or snapshot)
- `lastSnapshotIndex int` // last index we snapshotted up to

### KV trigger

After each `processApplyMsg`, under `s.mu`:
- If `s.rf.SnapshotThreshold() > 0` AND
  `(s.lastApplied - s.lastSnapshotIndex) >= s.rf.SnapshotThreshold()`:
  - Serialize `{s.state, non-expired s.duplicates}` to bytes.
  - Call `s.rf.Snapshot(s.lastApplied, bytes)`.
  - Set `s.lastSnapshotIndex = s.lastApplied`.
- Threshold `0` = disabled (current behavior; no snapshots).

Snapshot payload schema (JSON):
```json
{
  "state": {"k": "v", ...},
  "dedup": {
    "<clientId>": {
      "<seqNum>": {"result": {...}, "timestamp": "..."}
    }
  }
}
```

### Tests (S1)

`raft/persister_test.go`:
- `MemoryPersister` snapshot round-trip + missing-snapshot.
- `FilePersister` snapshot round-trip + missing-file + parent-dir creation.

`raft/snapshot_internal_test.go` (whitebox, `package raft`):
- `Snapshot` truncates log, bumps logBase, persists.
- `Snapshot` rejects uncommitted index (`index > commitIndex`).
- `Snapshot` is no-op when `index <= logBase`.
- `loadPersistedState` with snapshot sets commitIndex/lastApplied floor
  and sends a SnapshotValid ApplyMsg through applyCh.
- `loadPersistedState` without snapshot keeps current behavior.

`kv/apply_internal_test.go`:
- applyLoop handles SnapshotValid: replaces state + dedup, sets
  lastApplied, clears recent, notifies no waiter.

`server/server_test.go` (blackbox, end-to-end — the gap-closing test):
- 1-node cluster, `snapshot_threshold` set, write >threshold entries,
  Shutdown, restart with same data_dir, Get returns the persisted value.

All `-race`, `t.TempDir()` for disk state, `require.Eventually` for async.

---

## Step S2 — `InstallSnapshot` RPC (leader → lagging follower)

### Leader side

In `sendAppendEntries` (`raft/replication.go:147`), after computing
`prevLogIdx := nextIndex - 1`:
- If `prevLogIdx < r.logBase` (peer is behind the snapshot): call
  `go r.sendInstallSnapshot(peerId, term)` instead of building an
  AppendEntries request. Return early.

New `sendInstallSnapshot(peerId int, term int)`:
- Dial lazily, snapshot `r.snapshot`/`r.snapshotIndex`/`r.snapshotTerm`
  under `r.mu`.
- Send `InstallSnapshotRequest{Term, LeaderId,
  LastIncludedIndex: snapshotIndex, LastIncludedTerm: snapshotTerm,
  Offset: 0, Data: snapshot, Done: true}` (single chunk).
- On reply: if `resp.Term > currentTerm` → stepDown. Else on success:
  set `peer.matchIndex = r.snapshotIndex`, `peer.nextIndex =
  r.snapshotIndex + 1`.

### Follower handler

Replace the `InstallSnapshot` stub at `raft/replication.go:410`.
Under `r.mu`:
- `req.Term < currentTerm` → respond `{Term: currentTerm}`, no-op.
- `req.Term > currentTerm` → `stepDown(req.Term)`.
- `req.LastIncludedIndex <= r.commitIndex` → discard (already applied
  past it); respond `{Term: currentTerm}`.
- Divergence guard: if we have the log entry at `LastIncludedIndex`
  (i.e. `LastIncludedIndex - logBase - 1` is in range) and its term !=
  `LastIncludedTerm` → discard (AGENTS.md `CondInstallSnapshot` rule).
- Else install:
  - Set `r.logBase = req.LastIncludedIndex`.
  - Truncate `r.log` to entries with `Index > LastIncludedIndex`.
  - Set `r.snapshot = req.Data`, `r.snapshotIndex/Term` from the
    request.
  - Set `r.commitIndex = r.lastApplied = req.LastIncludedIndex`.
  - Persist (raft-state + snapshot).
  - Send `ApplyMsg{SnapshotValid:true, ...}` through `applyCh` so the
    follower's KV rehydrates.
- Respond `{Term: r.currentTerm}`.

### ~4MB cap

Add `maxSnapshotBytes` const (e.g. 4 * 1024 * 1024). Enforce in:
- `Raft.Snapshot()` — reject oversize `data` with a clear error.
- `InstallSnapshot` handler — reject oversize `req.Data` (respond
  `codes.InvalidArgument`).

### Tests (S2)

`raft/snapshot_internal_test.go`:
- `InstallSnapshot` follower installs + rehydrates + advances
  commitIndex + truncates log + bumps logBase.
- `InstallSnapshot` rejects stale term.
- `InstallSnapshot` discards when `last_included_index <= commitIndex`.
- `InstallSnapshot` discards on term mismatch at the index.
- Oversize data rejected.

`raft/replication_internal_test.go`:
- Leader falls back to `sendInstallSnapshot` when `nextIndex <= logBase`.
- After InstallSnapshot success, AppendEntries resumes from
  `snapshotIndex + 1`.

`raft/snapshot_test.go` (blackbox, `package raft_test`, 3-node harness):
- Partition one follower past the threshold; leader snapshots; follower
  rejoins; caught up via InstallSnapshot; subsequent AppendEntries
  resume; KV ops consistent across the cluster.

All `-race`.

---

## Sequencing & rollout

1. `0.2.0` created off `main` (done). README working-tree edits (em-dash
   removals) are the user's local changes — leave untouched unless asked.
2. Implement S1 (TDD red→green: tests first, then impl). Run full
   `-race` suite + vet + fmt. PR `0.2.0` → `dev`, review/merge, dev → main.
3. Fast-forward `0.2.0` to main, implement S2 (same cadence), PR, merge.
4. After both land: update AGENTS.md "Snapshotting (deferred)" →
   "Snapshotting" (mark S1/S2 as shipped), README snapshotting section
   (drop "deferred"/"isn't implemented yet" framing).

## Out of scope for 0.2.0

- Streamed/chunked InstallSnapshot (single chunk + 4MB cap is enough).
- Automatic snapshot transfer on leader change (leaders push snapshots
  on the next AppendEntries when `nextIndex > logBase`; election-time
  catch-up is handled by the existing `nextIndex = lastLogIdx+1` reset).
- Snapshot throttling / scheduling — threshold trigger is sufficient.
- Compacting the dedup cache beyond the per-client cap (already handled
  by `cleanupDuplicateCacheLocked`).