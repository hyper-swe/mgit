package worktreesync

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree materializes path->content pairs under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
}

// readFile is a terse helper for assertions.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	return string(b)
}

// TestBuildManifest_HashesContentAndMode verifies identical content yields
// identical entries and any change — content or mode — is visible.
func TestBuildManifest_HashesContentAndMode(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package a", "sub/b.go": "package b"})

	got, err := BuildManifest(root)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Contains(t, got, "a.go")
	assert.Contains(t, got, filepath.Join("sub", "b.go"))
	assert.NotEqual(t, got["a.go"].Hash, got[filepath.Join("sub", "b.go")].Hash)

	same, err := BuildManifest(root)
	require.NoError(t, err)
	assert.Equal(t, got, same, "hashing is deterministic")
}

// TestBuildManifest_SkipsThePrivateStore verifies the guest's own .mgit never
// enters a manifest, so it can never be planned for update or deletion.
// Refs: SEC-03, MGIT-71
func TestBuildManifest_SkipsThePrivateStore(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "x", ".mgit/HEAD": "ref: refs/heads/task"})

	got, err := BuildManifest(root)

	require.NoError(t, err)
	assert.Contains(t, got, "a.go")
	for path := range got {
		assert.NotContains(t, path, ".mgit", "the private store is out of scope")
	}
}

// TestBuildManifest_AbsentTree_IsEmptyNotAnError verifies a not-yet-created
// tree reads as empty, so the first sync of a fresh sandbox is an ordinary
// case rather than a failure.
func TestBuildManifest_AbsentTree_IsEmptyNotAnError(t *testing.T) {
	got, err := BuildManifest(filepath.Join(t.TempDir(), "nope"))

	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestApply_WritesUpdatesAndRemovesDeletes verifies the plan is realized in
// the guest's tree: new content arrives, deleted paths go.
func TestApply_WritesUpdatesAndRemovesDeletes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"a.go": "NEW", "sub/c.go": "FRESH"})
	writeTree(t, dst, map[string]string{"a.go": "OLD", "gone.go": "bye"})

	err := Apply(src, dst, Plan{Update: []string{"a.go", "sub/c.go"}, Delete: []string{"gone.go"}})

	require.NoError(t, err)
	assert.Equal(t, "NEW", readFile(t, filepath.Join(dst, "a.go")))
	assert.Equal(t, "FRESH", readFile(t, filepath.Join(dst, "sub", "c.go")))
	_, statErr := os.Stat(filepath.Join(dst, "gone.go"))
	assert.True(t, os.IsNotExist(statErr))
}

// TestApply_LeavesGuestCreatedPathsAlone verifies apply touches only what the
// plan names — the property that keeps node_modules alive.
func TestApply_LeavesGuestCreatedPathsAlone(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"a.go": "NEW"})
	writeTree(t, dst, map[string]string{"a.go": "OLD", "node_modules/dep/index.js": "cached"})

	require.NoError(t, Apply(src, dst, Plan{Update: []string{"a.go"}}))

	assert.Equal(t, "cached", readFile(t, filepath.Join(dst, "node_modules", "dep", "index.js")))
}

// TestApply_RefusesABlockedPlan verifies a conflicted plan cannot be applied
// by accident — the all-or-nothing guarantee is enforced at the apply step and
// not left to the caller's discipline. Refs: MGIT-71
func TestApply_RefusesABlockedPlan(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"a.go": "NEW"})
	writeTree(t, dst, map[string]string{"a.go": "GUEST-EDIT"})
	blocked := Plan{Update: []string{"a.go"}, Conflicts: []Conflict{{Path: "a.go", Reason: ReasonModifiedInGuest}}}

	err := Apply(src, dst, blocked)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, "GUEST-EDIT", readFile(t, filepath.Join(dst, "a.go")), "nothing is applied")
}

// TestApply_RefusesAPathEscapingTheTree verifies a traversing path in a plan
// cannot write outside the guest's worktree. The plans this package computes
// are host-derived, so this is defense in depth against a future caller
// building one from less trustworthy input. Refs: SEC-03
func TestApply_RefusesAPathEscapingTheTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("host secret"), 0o600))

	for _, bad := range []string{"../escape.txt", "sub/../../escape.txt", "/etc/passwd"} {
		err := Apply(src, dst, Plan{Update: []string{bad}})
		require.Error(t, err, "path %q must be refused", bad)
		assert.ErrorIs(t, err, ErrUnsafePath)
	}
	for _, bad := range []string{"../escape.txt", "/etc/passwd"} {
		err := Apply(src, dst, Plan{Delete: []string{bad}})
		require.Error(t, err, "delete of %q must be refused", bad)
		assert.ErrorIs(t, err, ErrUnsafePath)
	}
}

// TestApply_EmptyPlan_IsANoOp verifies the common case costs nothing.
func TestApply_EmptyPlan_IsANoOp(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, dst, map[string]string{"a.go": "UNCHANGED"})

	require.NoError(t, Apply(src, dst, Plan{}))

	assert.Equal(t, "UNCHANGED", readFile(t, filepath.Join(dst, "a.go")))
}

// TestApply_PreservesModes verifies an executable bit crosses with the file,
// since a mode change is one of the edits sync carries.
func TestApply_PreservesModes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeTree(t, src, map[string]string{"run.sh": "#!/bin/sh"})
	require.NoError(t, os.Chmod(filepath.Join(src, "run.sh"), 0o700)) //nolint:gosec // the executable bit is the property under test

	require.NoError(t, Apply(src, dst, Plan{Update: []string{"run.sh"}}))

	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// TestApply_Delete_LeavesNoOldContentBehind is the MGIT-90 property, expressed
// where it can be tested without a VM: a deleted path must not still hold its
// old bytes on the way out. The e2e that motivated it can only run on real
// hardware, but the mechanism — empty it, then unlink it — is checkable here by
// holding the file open across the delete, which is exactly what a guest with a
// cached name lookup is doing.
//
// Without the truncate, this reads the old content back through the open
// descriptor; with it, the reader gets nothing. Refs: MGIT-90, ADR-011
func TestApply_Delete_LeavesNoOldContentBehind(t *testing.T) {
	staged, guest := t.TempDir(), t.TempDir()
	doomed := filepath.Join(guest, "secret.txt")
	require.NoError(t, os.WriteFile(doomed, []byte("SECRET-OLD-CONTENT"), 0o600))

	// A reader that resolved the name BEFORE the delete, like a guest whose
	// kernel cached the lookup.
	held, err := os.Open(doomed) //nolint:gosec // G304: a t.TempDir path this test just wrote
	require.NoError(t, err)
	defer func() { _ = held.Close() }()

	require.NoError(t, Apply(staged, guest, Plan{Delete: []string{"secret.txt"}}))

	got, err := io.ReadAll(held)
	require.NoError(t, err, "the held descriptor must still be readable; the point is what it reads")
	assert.Empty(t, string(got),
		"a reader holding the path across a delete must not get the old content back")

	_, err = os.Stat(doomed)
	assert.True(t, os.IsNotExist(err), "and the path itself must be gone")
}

// TestApply_Delete_MissingPath_IsNotAnError keeps the delete idempotent: a
// guest that already removed the file itself must not fail the sync.
func TestApply_Delete_MissingPath_IsNotAnError(t *testing.T) {
	staged, guest := t.TempDir(), t.TempDir()
	assert.NoError(t, Apply(staged, guest, Plan{Delete: []string{"never-existed.txt"}}))
}
