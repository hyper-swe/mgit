package land

import (
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// blobObject builds a land blob object and returns it with its git hash.
func blobObject(t *testing.T, content string) (Object, string) {
	t.Helper()
	o := &plumbing.MemoryObject{}
	o.SetType(plumbing.BlobObject)
	_, err := o.Write([]byte(content))
	require.NoError(t, err)
	r, err := o.Reader()
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return Object{Type: ObjBlob, Data: data}, o.Hash().String()
}

// treeObject builds a land tree object from entries and returns its hash.
func treeObject(t *testing.T, entries []object.TreeEntry) (Object, string) {
	t.Helper()
	tr := &object.Tree{Entries: entries}
	enc := &plumbing.MemoryObject{}
	require.NoError(t, tr.Encode(enc))
	r, err := enc.Reader()
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return Object{Type: ObjTree, Data: data}, enc.Hash().String()
}

// commitObject builds a land commit object over a tree and returns the
// matching (identity/metadata-consistent) model.Commit, sans FileDiffs.
func tbCommit(t *testing.T, treeHash, parent string) (Object, *model.Commit) {
	t.Helper()
	when := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	sig := object.Signature{Name: "agent-1", Email: "a@x", When: when}
	gc := &object.Commit{Author: sig, Committer: sig, Message: "feat: land", TreeHash: plumbing.NewHash(treeHash)}
	if parent != "" {
		gc.ParentHashes = []plumbing.Hash{plumbing.NewHash(parent)}
	}
	enc := &plumbing.MemoryObject{}
	require.NoError(t, gc.Encode(enc))
	r, err := enc.Reader()
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	c := &model.Commit{
		CommitID: enc.Hash().String(), Message: "feat: land", CreatedAt: when,
		ParentID: parent, TreeHash: treeHash, AgentID: "agent-1",
	}
	return Object{Type: ObjCommit, Data: data}, c
}

// TestVerifyTreeBinding_InitialCommit_MatchingDiff verifies a single-file
// initial commit whose claimed diff matches the landed tree passes.
func TestVerifyTreeBinding_InitialCommit_MatchingDiff(t *testing.T) {
	blob, blobHash := blobObject(t, "hello\n")
	tree, treeHash := treeObject(t, []object.TreeEntry{
		{Name: "foo.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(blobHash)},
	})
	commit, c := tbCommit(t, treeHash, "")
	c.FileDiffs = []model.FileDiff{{Path: "foo.txt", Operation: model.DiffAdded, NewHash: blobHash}}

	require.NoError(t, VerifyTreeBinding([]Object{blob, tree, commit}, c, nil))
}

// TestVerifyTreeBinding_ModifiedAddedDeleted verifies the recomputed diff
// against a parent file set covers modify/add/delete, and that an unchanged
// file's blob need not be re-sent.
func TestVerifyTreeBinding_ModifiedAddedDeleted(t *testing.T) {
	_, keepOld := blobObject(t, "keep\n")   // unchanged: blob stays in parent store
	_, modOld := blobObject(t, "mod-old\n") // parent content of mod.txt
	_, delOld := blobObject(t, "gone\n")    // parent content of del.txt
	modNew, modNewHash := blobObject(t, "mod-new\n")
	addNew, addNewHash := blobObject(t, "added\n")

	tree, treeHash := treeObject(t, []object.TreeEntry{
		{Name: "add.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(addNewHash)},
		{Name: "keep.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(keepOld)},
		{Name: "mod.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(modNewHash)},
	})
	commit, c := tbCommit(t, treeHash, "")
	c.FileDiffs = []model.FileDiff{
		{Path: "mod.txt", Operation: model.DiffModified, OldHash: modOld, NewHash: modNewHash},
		{Path: "del.txt", Operation: model.DiffDeleted, OldHash: delOld},
		{Path: "add.txt", Operation: model.DiffAdded, NewHash: addNewHash},
	}
	parent := map[string]string{"keep.txt": keepOld, "mod.txt": modOld, "del.txt": delOld}

	// Only the changed blobs (mod, add) are in the pool; keep's is not.
	require.NoError(t, VerifyTreeBinding([]Object{modNew, addNew, tree, commit}, c, parent))
}

// TestVerifyTreeBinding_ClaimedDiffMismatch_Fails verifies a claimed diff
// that does not describe the real tree fails the land.
func TestVerifyTreeBinding_ClaimedDiffMismatch_Fails(t *testing.T) {
	blob, blobHash := blobObject(t, "hello\n")
	tree, treeHash := treeObject(t, []object.TreeEntry{
		{Name: "foo.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(blobHash)},
	})
	commit, c := tbCommit(t, treeHash, "")
	// Lie: claim a different path was added than the tree actually contains.
	c.FileDiffs = []model.FileDiff{{Path: "evil.txt", Operation: model.DiffAdded, NewHash: blobHash}}

	err := VerifyTreeBinding([]Object{blob, tree, commit}, c, nil)
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestVerifyTreeBinding_PathTraversal_Rejected verifies a tree entry whose
// path escapes the worktree is rejected (ValidateTreePath at the boundary).
func TestVerifyTreeBinding_PathTraversal_Rejected(t *testing.T) {
	blob, blobHash := blobObject(t, "payload\n")
	tree, treeHash := treeObject(t, []object.TreeEntry{
		{Name: "..", Mode: filemode.Regular, Hash: plumbing.NewHash(blobHash)},
	})
	commit, c := tbCommit(t, treeHash, "")
	c.FileDiffs = []model.FileDiff{{Path: "..", Operation: model.DiffAdded, NewHash: blobHash}}

	err := VerifyTreeBinding([]Object{blob, tree, commit}, c, nil)
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestVerifyTreeBinding_NewBlobAbsent_Fails verifies a tree referencing a
// blob that never landed is rejected (no dangling object).
func TestVerifyTreeBinding_NewBlobAbsent_Fails(t *testing.T) {
	_, blobHash := blobObject(t, "never-sent\n") // hash only; blob omitted from pool
	tree, treeHash := treeObject(t, []object.TreeEntry{
		{Name: "foo.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(blobHash)},
	})
	commit, c := tbCommit(t, treeHash, "")
	c.FileDiffs = []model.FileDiff{{Path: "foo.txt", Operation: model.DiffAdded, NewHash: blobHash}}

	err := VerifyTreeBinding([]Object{tree, commit}, c, nil) // blob deliberately absent
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestVerifyTreeBinding_NestedSubtree verifies recursion into subtrees and
// that nested paths are validated and diffed by full path.
func TestVerifyTreeBinding_NestedSubtree(t *testing.T) {
	blob, blobHash := blobObject(t, "nested\n")
	subtree, subHash := treeObject(t, []object.TreeEntry{
		{Name: "b.txt", Mode: filemode.Regular, Hash: plumbing.NewHash(blobHash)},
	})
	root, rootHash := treeObject(t, []object.TreeEntry{
		{Name: "a", Mode: filemode.Dir, Hash: plumbing.NewHash(subHash)},
	})
	commit, c := tbCommit(t, rootHash, "")
	c.FileDiffs = []model.FileDiff{{Path: "a/b.txt", Operation: model.DiffAdded, NewHash: blobHash}}

	require.NoError(t, VerifyTreeBinding([]Object{blob, subtree, root, commit}, c, nil))
}

// TestVerifyTreeBinding_EmptyTree verifies a commit with no files (zero
// tree) and no claimed diffs passes without a tree object in the pool.
func TestVerifyTreeBinding_EmptyTree(t *testing.T) {
	commit, c := tbCommit(t, plumbing.ZeroHash.String(), "")
	c.FileDiffs = nil
	require.NoError(t, VerifyTreeBinding([]Object{commit}, c, nil))
}

// TestVerifyTreeBinding_NilCommit verifies the nil guard.
func TestVerifyTreeBinding_NilCommit(t *testing.T) {
	assert.ErrorIs(t, VerifyTreeBinding(nil, nil, nil), model.ErrLandVerificationFailed)
}
