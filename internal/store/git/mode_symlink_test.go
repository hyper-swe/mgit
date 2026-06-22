package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestCreateCommit_ExecutableBit_PreservedInTree verifies that committing a
// file with the executable bit (0o755) records a 100755 entry in the tree, not
// 100644 — git's regular/executable distinction must survive the
// staging→tree pipeline so mgit's tree hash equals git's. Refs: MGIT-14.7 (#3)
func TestCreateCommit_ExecutableBit_PreservedInTree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	script := filepath.Join(repo.Root(), "run.sh")
	writeExecutable(t, script, "#!/bin/sh\necho hi\n")
	require.NoError(t, ws.Add(ctx, "run.sh"))

	c := makeTestModelCommit(t, "MGIT-7.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	mode := treeEntryMode(t, repo, hash, "run.sh")
	assert.Equal(t, filemode.Executable, mode, "0o755 file must commit as 100755")
}

// TestCheckout_ExecutableBit_RestoredOnDisk verifies the round-trip: a 0o755
// file committed on one branch is restored with its executable bit set when
// checked out, not clobbered to 0o600. Refs: MGIT-14.7 (#3)
func TestCheckout_ExecutableBit_RestoredOnDisk(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	script := filepath.Join(repo.Root(), "run.sh")
	writeExecutable(t, script, "#!/bin/sh\necho hi\n")
	require.NoError(t, ws.Add(ctx, "run.sh"))
	c := makeTestModelCommit(t, "MGIT-7.2")
	head, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	require.NoError(t, createBranchAt(t, bs, "task/MGIT-7.3", head, "MGIT-7.3"))

	// Destroy the on-disk file, then checkout must restore content AND mode.
	require.NoError(t, os.Remove(script))
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-7.3"))

	info, err := os.Lstat(script)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o100, "checkout must restore the executable bit")
}

// TestCreateCommit_Symlink_StoredAsLinkNotTarget verifies that a working-tree
// symlink is committed as a git symlink entry (mode 120000) whose blob holds
// the LINK TEXT, never the target file's content. This closes the exfiltration
// + git-divergence defect. Refs: MGIT-14.7 (#2)
func TestCreateCommit_Symlink_StoredAsLinkNotTarget(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	secret := filepath.Join(repo.Root(), "secret.txt")
	secretContent := "TOP-SECRET-CONTENT-MUST-NOT-LEAK\n"
	require.NoError(t, os.WriteFile(secret, []byte(secretContent), 0o600))

	link := filepath.Join(repo.Root(), "link")
	require.NoError(t, os.Symlink("secret.txt", link))

	require.NoError(t, ws.Add(ctx, "link"))
	c := makeTestModelCommit(t, "MGIT-8.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	// The link entry is a symlink, not a regular blob.
	mode := treeEntryMode(t, repo, hash, "link")
	assert.Equal(t, filemode.Symlink, mode, "symlink must commit as 120000")

	// The blob content is the link TEXT, not the target's content.
	blob, err := cs.GetFileFromCommit(ctx, hash, "link")
	require.NoError(t, err)
	assert.Equal(t, "secret.txt", string(blob), "symlink blob must hold link text")
	assert.NotContains(t, string(blob), "TOP-SECRET", "target content must not leak into the blob")
}

// TestCheckout_Symlink_RecreatedAsLink verifies a committed symlink is restored
// on checkout as an actual symlink (not a regular file holding the link text).
// Refs: MGIT-14.7 (#2, #3)
func TestCheckout_Symlink_RecreatedAsLink(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "target.txt"), []byte("data\n"), 0o600))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(repo.Root(), "link")))
	require.NoError(t, ws.Add(ctx, "link"))
	c := makeTestModelCommit(t, "MGIT-8.2")
	head, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)

	require.NoError(t, createBranchAt(t, bs, "task/MGIT-8.3", head, "MGIT-8.3"))
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "link")))
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-8.3"))

	info, err := os.Lstat(filepath.Join(repo.Root(), "link"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "checkout must recreate the symlink, not a regular file")
	dst, err := os.Readlink(filepath.Join(repo.Root(), "link"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", dst)
}

// TestCreateCommit_ModeVariedTree_HashMatchesGoGit is the strong provenance
// assertion: mgit's computed root-tree hash for a tree containing a regular
// file, an executable file, and a symlink EQUALS the hash go-git computes for
// the identical set of entries. This proves mgit emits git-faithful modes.
// Refs: MGIT-14.7 (#2, #3)
func TestCreateCommit_ModeVariedTree_HashMatchesGoGit(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "regular.txt"), []byte("plain\n"), 0o600))
	writeExecutable(t, filepath.Join(repo.Root(), "run.sh"), "#!/bin/sh\n")
	require.NoError(t, os.Symlink("regular.txt", filepath.Join(repo.Root(), "link")))
	require.NoError(t, ws.Add(ctx, "regular.txt"))
	require.NoError(t, ws.Add(ctx, "run.sh"))
	require.NoError(t, ws.Add(ctx, "link"))

	c := makeTestModelCommit(t, "MGIT-9.1")
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	mgitTree := treeOfCommit(t, repo, hash)

	// Independently build the SAME tree via go-git's own blob + tree encoding.
	want := goGitTreeHash(t, []object.TreeEntry{
		{Name: "link", Mode: filemode.Symlink, Hash: goGitBlobHash(t, []byte("regular.txt"))},
		{Name: "regular.txt", Mode: filemode.Regular, Hash: goGitBlobHash(t, []byte("plain\n"))},
		{Name: "run.sh", Mode: filemode.Executable, Hash: goGitBlobHash(t, []byte("#!/bin/sh\n"))},
	})
	assert.Equal(t, want.String(), mgitTree, "mgit tree hash must equal go-git's for a mode-varied tree")
}

// TestCheckout_DirtyWorktree_RefusesAtStore verifies the store-level checkout
// guard (defense in depth): a direct WorktreeStore.Checkout on a worktree with
// uncommitted user changes must refuse rather than clobber them. Refs:
// MGIT-14.7 (#4)
func TestCheckout_DirtyWorktree_RefusesAtStore(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "f.go"), []byte("v1\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "f.go"))
	c := makeTestModelCommit(t, "MGIT-10.1")
	head, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	require.NoError(t, createBranchAt(t, bs, "task/MGIT-10.2", head, "MGIT-10.2"))

	// Introduce an uncommitted change to a tracked file.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "f.go"), []byte("DIRTY-UNCOMMITTED\n"), 0o600))

	err = ws.Checkout(ctx, "task/MGIT-10.2")
	require.Error(t, err, "direct checkout must refuse on a dirty worktree")

	// The uncommitted change must NOT have been clobbered.
	data, readErr := os.ReadFile(filepath.Join(repo.Root(), "f.go"))
	require.NoError(t, readErr)
	assert.Equal(t, "DIRTY-UNCOMMITTED\n", string(data), "dirty content must be preserved")
}

// TestCheckout_RemovesTrackedFileAbsentInTarget verifies checkout removes a
// file tracked at the old HEAD but absent from the target branch tree (clean
// worktree path). Refs: MGIT-14.7 (#4 coverage)
func TestCheckout_RemovesTrackedFileAbsentInTarget(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	// main has only base.go.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "base.go"), []byte("base\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "base.go"))
	mainHead, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-11.1"))
	require.NoError(t, err)
	require.NoError(t, createBranchAt(t, bs, "task/MGIT-11.2", mainHead, "MGIT-11.2"))

	// On the task branch, add extra.go and commit.
	require.NoError(t, bs.SwitchBranch(ctx, "task/MGIT-11.2"))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "extra.go"), []byte("extra\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "extra.go"))
	_, err = cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-11.3"))
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(repo.Root(), "extra.go"))

	// Back to main: extra.go is absent from main's tree → must be removed.
	require.NoError(t, ws.Checkout(ctx, "main"))
	_, statErr := os.Stat(filepath.Join(repo.Root(), "extra.go"))
	assert.True(t, os.IsNotExist(statErr), "checkout must remove a tracked file absent from the target tree")
}

// TestCreateCommit_ConcurrentRefAdvance_FailsLoudly verifies branch-ref
// advancement is CAS-guarded: when two commits race off the same HEAD, the
// later advance that still believes the ref is at the old value must fail
// (stale) rather than silently orphan the earlier commit. Refs: MGIT-14.7 (#5)
func TestCreateCommit_ConcurrentRefAdvance_FailsLoudly(t *testing.T) {
	repo := initTestRepo(t)

	headRef, err := repo.repo.Head()
	require.NoError(t, err)
	branch := headRef.Name()
	base := headRef.Hash()

	// Build two distinct commit objects that both parent off `base`.
	commitA := buildChildCommit(t, repo, base, "A")
	commitB := buildChildCommit(t, repo, base, "B")

	// First CAS advance (base → A) must succeed.
	require.NoError(t, casAdvanceRef(repo, branch, commitA, base))

	// A second advance that still believes the ref is at `base` must FAIL,
	// because the stored value is now A — this is the lost-update guard.
	err = casAdvanceRef(repo, branch, commitB, base)
	require.Error(t, err, "stale CAS advance must fail loudly, not orphan the earlier commit")

	// The branch must still point at A (the winner), never B.
	cur, err := repo.repo.Storer.Reference(branch)
	require.NoError(t, err)
	assert.Equal(t, commitA, cur.Hash())
}

// TestCheckout_RegularReplacedBySymlink verifies checkout cleanly replaces an
// existing regular file at a path with a symlink committed on the target
// branch (the file↔symlink switch). Refs: MGIT-14.7 (#2, #3)
func TestCheckout_RegularReplacedBySymlink(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)

	// main: "p" is a regular file.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "p"), []byte("regular\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "tgt.txt"), []byte("t\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "p"))
	require.NoError(t, ws.Add(ctx, "tgt.txt"))
	mainHead, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-12.1"))
	require.NoError(t, err)
	require.NoError(t, createBranchAt(t, bs, "task/MGIT-12.2", mainHead, "MGIT-12.2"))

	// task branch: replace "p" with a symlink to tgt.txt.
	require.NoError(t, bs.SwitchBranch(ctx, "task/MGIT-12.2"))
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "p")))
	require.NoError(t, os.Symlink("tgt.txt", filepath.Join(repo.Root(), "p")))
	require.NoError(t, ws.Add(ctx, "p"))
	_, err = cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-12.3"))
	require.NoError(t, err)

	// Switch back to main: "p" must become a regular file again.
	require.NoError(t, ws.Checkout(ctx, "main"))
	info, err := os.Lstat(filepath.Join(repo.Root(), "p"))
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink, "main's regular file must replace the symlink")
	data, err := os.ReadFile(filepath.Join(repo.Root(), "p"))
	require.NoError(t, err)
	assert.Equal(t, "regular\n", string(data))

	// Switch to the task branch: "p" must become a symlink, replacing the file.
	require.NoError(t, ws.Checkout(ctx, "task/MGIT-12.2"))
	info, err = os.Lstat(filepath.Join(repo.Root(), "p"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "task branch's symlink must replace the regular file")
}

// TestCheckout_EscapingTargetPath_AbortsBeforeAnyWrite verifies the
// partial-checkout guard (#7): when a target tree contains a path that escapes
// the project root, materialization validates ALL paths up front and refuses,
// writing NO files at all — not even the legitimate sibling entries.
// Refs: MGIT-14.7 (#7)
func TestCheckout_EscapingTargetPath_AbortsBeforeAnyWrite(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	bs := NewBranchStore(repo)

	// Forge a commit whose tree holds a legit file AND an escaping path.
	st := repo.repo.Storer
	goodBlob, err := writeBlob(st, []byte("legit\n"))
	require.NoError(t, err)
	evilBlob, err := writeBlob(st, []byte("evil\n"))
	require.NoError(t, err)
	tree, err := writeNestedTree(st, map[string]blobEntry{
		"good.txt":          {hash: goodBlob, mode: filemode.Regular},
		"../escape-out.txt": {hash: evilBlob, mode: filemode.Regular},
	})
	require.NoError(t, err)
	base, err := repo.repo.Head()
	require.NoError(t, err)
	forged, err := writeCommit(st, commitParams{
		tree:     tree,
		parents:  []plumbing.Hash{base.Hash()},
		message:  "forged escaping tree",
		authorAt: object.Signature{Name: "t", Email: "t@mgit", When: repo.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, createBranchAt(t, bs, "task/MGIT-13.1", forged.String(), "MGIT-13.1"))

	err = ws.Checkout(ctx, "task/MGIT-13.1")
	require.Error(t, err, "checkout of a tree with an escaping path must fail")

	// Neither the escaping file nor the legitimate sibling was written.
	_, statErr := os.Stat(filepath.Join(repo.Root(), "good.txt"))
	assert.True(t, os.IsNotExist(statErr), "no file may be written when validation aborts the checkout")
	parent := filepath.Dir(repo.Root())
	_, escErr := os.Stat(filepath.Join(parent, "escape-out.txt"))
	assert.True(t, os.IsNotExist(escErr), "an escaping path must never be written outside the root")
}

// --- test-only helpers (no fault-injecting storer) ---

// writeExecutable writes content to path and sets the owner-executable bit.
// An executable test fixture is the whole point of the mode-preservation
// tests; the exec bit is required, so gosec's 0600-ceiling rule is suppressed.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	//nolint:gosec // G302: an executable fixture is required to test mode preservation
	require.NoError(t, os.Chmod(path, 0o700))
}

func createBranchAt(t *testing.T, bs *BranchStore, name, head, taskID string) error {
	t.Helper()
	tid, err := model.ParseTaskID(taskID)
	require.NoError(t, err)
	return bs.CreateBranch(context.Background(), &model.Branch{Name: name, HeadCommit: head, TaskID: tid})
}

// treeEntryMode resolves the filemode of a single path in a commit's tree.
func treeEntryMode(t *testing.T, repo *Repository, commitHash, path string) filemode.FileMode {
	t.Helper()
	c, err := repo.repo.CommitObject(hashFromString(commitHash))
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	entry, err := tree.FindEntry(path)
	require.NoError(t, err)
	return entry.Mode
}

// goGitBlobHash computes the blob hash go-git would assign to content, without
// involving any mgit code path.
func goGitBlobHash(t *testing.T, content []byte) plumbing.Hash {
	t.Helper()
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	_, err := obj.Write(content)
	require.NoError(t, err)
	return obj.Hash()
}

// goGitTreeHash computes the tree hash go-git would assign to the given
// entries, independent of mgit's writeNestedTree.
func goGitTreeHash(t *testing.T, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()
	tree := &object.Tree{Entries: entries}
	obj := &plumbing.MemoryObject{}
	require.NoError(t, tree.Encode(obj))
	return obj.Hash()
}

// buildChildCommit stores a distinct commit object parented off `parent`,
// keyed by `tag` so two calls produce different hashes. Returns its hash.
func buildChildCommit(t *testing.T, repo *Repository, parent plumbing.Hash, tag string) plumbing.Hash {
	t.Helper()
	parentCommit, err := repo.repo.CommitObject(parent)
	require.NoError(t, err)
	h, err := writeCommit(repo.repo.Storer, commitParams{
		tree:     parentCommit.TreeHash,
		parents:  []plumbing.Hash{parent},
		message:  "child " + tag,
		authorAt: object.Signature{Name: "t", Email: "t@mgit", When: repo.Now()},
	})
	require.NoError(t, err)
	return h
}

// casAdvanceRef performs a compare-and-set advance of branch from old→new using
// the production helper, so the test exercises the same CAS path commit.go uses.
func casAdvanceRef(repo *Repository, branch plumbing.ReferenceName, newHash, oldHash plumbing.Hash) error {
	return advanceBranchRefCAS(repo.repo.Storer, branch, newHash, oldHash)
}
