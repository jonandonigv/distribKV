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
