package raft_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jonandonigv/distribKV/raft"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleLog builds a log with two entries exercising the base64 path used by
// FilePersister (commands with arbitrary bytes).
func sampleLog() []raft.LogEntry {
	return []raft.LogEntry{
		{Index: 1, Term: 1, Command: []byte("put foo bar")},
		{Index: 2, Term: 1, Command: []byte("append foo baz")},
	}
}

// runRoundTrip exercises Save/Load on a persister and asserts the loaded
// state matches what was saved. Used by both MemoryPersister and
// FilePersister tests.
func runRoundTrip(t *testing.T, p raft.Persister) {
	t.Helper()

	log := sampleLog()
	require.NoError(t, p.Save(7, 3, log))

	term, votedFor, loaded, err := p.Load()
	require.NoError(t, err)
	assert.Equal(t, 7, term)
	assert.Equal(t, 3, votedFor)
	require.Len(t, loaded, len(log))
	for i, e := range log {
		assert.Equal(t, e.Index, loaded[i].Index, "Index mismatch at %d", i)
		assert.Equal(t, e.Term, loaded[i].Term, "Term mismatch at %d", i)
		assert.Equal(t, e.Command, loaded[i].Command, "Command mismatch at %d", i)
	}
}

// runEmptyLog verifies empty-log + zero state round-trips cleanly. The
// FilePersister must persist an empty array (not null) so Load returns a
// usable zero-length slice.
func runEmptyLog(t *testing.T, p raft.Persister) {
	t.Helper()
	require.NoError(t, p.Save(0, -1, nil))

	term, votedFor, loaded, err := p.Load()
	require.NoError(t, err)
	assert.Equal(t, 0, term)
	assert.Equal(t, -1, votedFor)
	assert.Empty(t, loaded)
}

func TestMemoryPersister_RoundTrip(t *testing.T) {
	runRoundTrip(t, raft.NewMemoryPersister())
}

func TestMemoryPersister_EmptyLog(t *testing.T) {
	runEmptyLog(t, raft.NewMemoryPersister())
}

func TestMemoryPersister_FaultInjection(t *testing.T) {
	p := raft.NewMemoryPersister()
	boom := errors.New("disk on fire")
	p.SetSaveErr(boom)
	err := p.Save(1, 0, nil)
	require.ErrorIs(t, err, boom, "Save should surface injected error")

	// Subsequent Save with no injected error works again.
	p.SetSaveErr(nil)
	require.NoError(t, p.Save(1, 0, nil))
}

func TestFilePersister_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-state.json")
	p := raft.NewFilePersister(path)
	runRoundTrip(t, p)
}

func TestFilePersister_EmptyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-state.json")
	p := raft.NewFilePersister(path)
	runEmptyLog(t, p)
}

func TestFilePersister_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.json")
	p := raft.NewFilePersister(path)

	_, _, _, err := p.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestFilePersister_OverwriteOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-state.json")
	p := raft.NewFilePersister(path)

	require.NoError(t, p.Save(1, 5, sampleLog()))
	require.NoError(t, p.Save(2, 7, sampleLog()))

	term, votedFor, loaded, err := p.Load()
	require.NoError(t, err)
	assert.Equal(t, 2, term, "second Save should overwrite first")
	assert.Equal(t, 7, votedFor)
	require.Len(t, loaded, 2)
}

func TestFilePersister_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not valid json}"), 0644))

	p := raft.NewFilePersister(path)
	_, _, _, err := p.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// TestFilePersister_NoTempLeftover verifies the atomic-write invariant:
// after a successful Save, no .tmp file is left in the target directory.
// A half-written raft-state.json on a crash is prevented by the
// temp+fsync+rename sequence.
func TestFilePersister_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raft-state.json")
	p := raft.NewFilePersister(path)

	require.NoError(t, p.Save(1, 0, sampleLog()))

	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "raft-state.json should exist after Save")

	_, tmpStatErr := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(tmpStatErr),
		"no .tmp file should remain after a successful Save, got err=%v", tmpStatErr)
}

// TestFilePersister_FailedSaveLeavesNoTarget verifies that whenSave errors
// midway (here, because the target's parent directory is actually a file,
// so MkdirAll fails), no raft-state.json is created at the target path.
func TestFilePersister_FailedSaveLeavesNoTarget(t *testing.T) {
	dir := t.TempDir()
	badParent := filepath.Join(dir, "iamfile")
	require.NoError(t, os.WriteFile(badParent, []byte("x"), 0644))
	path := filepath.Join(badParent, "raft-state.json")

	p := raft.NewFilePersister(path)
	err := p.Save(1, 0, sampleLog())
	require.Error(t, err, "Save should fail when the target's parent is not a directory")

	// The path can't be stat'd, regardless of error category: in any
	// failure mode, no junk should appear at path or path+".tmp".
	for _, leftover := range []string{path, path + ".tmp"} {
		_, statErr := os.Stat(leftover)
		assert.Error(t, statErr, "no leftover file at %s", leftover)
	}
}

// ---------------------------------------------------------------------------
// Snapshot persistence (0.2.0 — see PLAN.md "Step S1").
// ---------------------------------------------------------------------------

var sampleSnapshot = []byte{0x00, 0x01, 0xff, 0xfe, 'k', 'v'} // arbitrary bytes incl. binary

// runSnapshotRoundTrip exercises SaveSnapshot/LoadSnapshot and asserts the
// loaded metadata + bytes match what was saved. Used by both persister
// implementations.
func runSnapshotRoundTrip(t *testing.T, p raft.Persister) {
	t.Helper()
	require.NoError(t, p.SaveSnapshot(sampleSnapshot, 42, 7))

	data, idx, term, err := p.LoadSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 42, idx)
	assert.Equal(t, 7, term)
	assert.Equal(t, sampleSnapshot, data)
}

func TestMemoryPersister_SnapshotRoundTrip(t *testing.T) {
	runSnapshotRoundTrip(t, raft.NewMemoryPersister())
}

// TestMemoryPersister_SnapshotOverwrite verifies a second SaveSnapshot
// replaces the first.
func TestMemoryPersister_SnapshotOverwrite(t *testing.T) {
	p := raft.NewMemoryPersister()
	require.NoError(t, p.SaveSnapshot([]byte("old"), 10, 1))
	require.NoError(t, p.SaveSnapshot([]byte("new"), 20, 2))

	data, idx, term, err := p.LoadSnapshot()
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), data)
	assert.Equal(t, 20, idx)
	assert.Equal(t, 2, term)
}

func TestMemoryPersister_LoadSnapshotMissing(t *testing.T) {
	p := raft.NewMemoryPersister()
	_, _, _, err := p.LoadSnapshot()
	require.Error(t, err, "LoadSnapshot on a fresh persister should error")
}

// TestMemoryPersister_StateAndSnapshotIndependent verifies Save (raft-state)
// and SaveSnapshot don't clobber each other — separate lifecycles per PLAN.md.
func TestMemoryPersister_StateAndSnapshotIndependent(t *testing.T) {
	p := raft.NewMemoryPersister()
	require.NoError(t, p.Save(5, 2, sampleLog()))
	require.NoError(t, p.SaveSnapshot(sampleSnapshot, 42, 7))

	term, votedFor, loaded, err := p.Load()
	require.NoError(t, err)
	assert.Equal(t, 5, term)
	assert.Equal(t, 2, votedFor)
	assert.Len(t, loaded, 2)

	_, idx, _, err := p.LoadSnapshot()
	require.NoError(t, err)
	assert.Equal(t, 42, idx)
}

func TestFilePersister_SnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.bin")
	p := raft.NewFilePersister(path)
	runSnapshotRoundTrip(t, p)
}

// TestFilePersister_SnapshotMissingFile mirrors the Load missing-file case:
// a fresh node has no snapshot yet and LoadSnapshot must say so cleanly.
func TestFilePersister_SnapshotMissingFile(t *testing.T) {
	dir := t.TempDir()
	p := raft.NewFilePersister(filepath.Join(dir, "nope.bin"))
	_, _, _, err := p.LoadSnapshot()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// TestFilePersister_SnapshotCreatesParentDirs verifies snapshot writes into
// a not-yet-existing data dir succeed (matches Save's MkdirAll behavior).
func TestFilePersister_SnapshotCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "snapshot.bin")
	p := raft.NewFilePersister(path)

	require.NoError(t, p.SaveSnapshot(sampleSnapshot, 9, 3))

	data, idx, term, err := p.LoadSnapshot()
	require.NoError(t, err)
	assert.Equal(t, sampleSnapshot, data)
	assert.Equal(t, 9, idx)
	assert.Equal(t, 3, term)
}

// TestFilePersister_SnapshotNoTempLeftover verifies the atomic-write
// invariant holds for snapshots too.
func TestFilePersister_SnapshotNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.bin")
	p := raft.NewFilePersister(path)

	require.NoError(t, p.SaveSnapshot(sampleSnapshot, 1, 1))

	_, tmpStatErr := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(tmpStatErr),
		"no .tmp file should remain after a successful SaveSnapshot, got err=%v", tmpStatErr)
}

// TestFilePersister_CorruptSnapshotHeader verifies a truncated/garbage
// snapshot file surfaces a decode error rather than garbage state.
func TestFilePersister_CorruptSnapshotHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.bin")
	require.NoError(t, os.WriteFile(path, []byte("garbage-not-a-header"), 0644))

	p := raft.NewFilePersister(path)
	_, _, _, err := p.LoadSnapshot()
	require.Error(t, err)
}
