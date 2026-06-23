package provision

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// seededRepo builds a host project at repoRoot with a task branch carrying one
// extra commit (a base the worktree would be materialized from) and returns
// the task ID, the base commit hash, and the blob content committed.
func seededRepo(t *testing.T) (repoRoot, taskID, baseHash, blobContent string) {
	t.Helper()
	repoRoot = t.TempDir()
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	taskID = "MGIT-11.6.8"
	blobContent = "base content"
	// Build a base commit on task/<id> directly in the shared store via go-git.
	shared := filesystem.NewStorage(osfs.New(filepath.Join(repoRoot, ".mgit")), cache.NewObjectLRUDefault())

	blob := shared.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	bw, err := blob.Writer()
	require.NoError(t, err)
	_, _ = bw.Write([]byte(blobContent))
	require.NoError(t, bw.Close())
	blobHash, err := shared.SetEncodedObject(blob)
	require.NoError(t, err)

	treeObj := shared.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
		{Name: "base.txt", Mode: 0o100644, Hash: blobHash},
	}}).Encode(treeObj))
	treeHash, err := shared.SetEncodedObject(treeObj)
	require.NoError(t, err)

	sig := object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()}
	commitObj := shared.NewEncodedObject()
	require.NoError(t, (&object.Commit{Author: sig, Committer: sig, Message: "base", TreeHash: treeHash}).Encode(commitObj))
	ch, err := shared.SetEncodedObject(commitObj)
	require.NoError(t, err)
	baseHash = ch.String()

	require.NoError(t, shared.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(model.TaskBranchName(taskID)), ch)))
	return repoRoot, taskID, baseHash, blobContent
}

// TestProvision_SeedsBaseCommitOnly proves the private store is seeded with
// exactly the task base commit's reachable pool and HEAD points at it.
func TestProvision_SeedsBaseCommitOnly(t *testing.T) {
	repoRoot, taskID, baseHash, blobContent := seededRepo(t)
	p, err := NewStoreProvisioner(repoRoot)
	require.NoError(t, err)

	privDir := filepath.Join(t.TempDir(), "private", ".mgit")
	ps, err := p.Provision(taskID, privDir)
	require.NoError(t, err)
	assert.Equal(t, privDir, ps.Dir)
	assert.Equal(t, filepath.Join(repoRoot, ".mgit"), ps.SharedDir)

	// Open the private store and confirm HEAD resolves to the seeded base, the
	// blob is present, and the committed content round-trips.
	priv := filesystem.NewStorage(osfs.New(privDir), cache.NewObjectLRUDefault())
	repo, err := gogit.Open(priv, nil)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, baseHash, head.Hash().String(), "private HEAD is the seeded base commit")
	assert.Equal(t, model.TaskBranchName(taskID), head.Name().Short(), "HEAD tracks the task branch")

	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	f, err := tree.File("base.txt")
	require.NoError(t, err)
	got, err := f.Contents()
	require.NoError(t, err)
	assert.Equal(t, blobContent, got)
}

// TestProvision_DoesNotCopyOtherBranchObjects proves the private store contains
// ONLY the task base pool — an object reachable only from a different branch in
// the shared store is absent (the SEC-03 cross-task non-exposure guarantee).
func TestProvision_DoesNotCopyOtherBranchObjects(t *testing.T) {
	repoRoot, taskID, _, _ := seededRepo(t)

	// Add an object reachable only from a foreign branch in the shared store.
	shared := filesystem.NewStorage(osfs.New(filepath.Join(repoRoot, ".mgit")), cache.NewObjectLRUDefault())
	secret := shared.NewEncodedObject()
	secret.SetType(plumbing.BlobObject)
	sw, err := secret.Writer()
	require.NoError(t, err)
	_, _ = sw.Write([]byte("OTHER TASK SECRET"))
	require.NoError(t, sw.Close())
	secretHash, err := shared.SetEncodedObject(secret)
	require.NoError(t, err)
	otherTree := shared.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
		{Name: "secret.txt", Mode: 0o100644, Hash: secretHash},
	}}).Encode(otherTree))
	otherTreeHash, err := shared.SetEncodedObject(otherTree)
	require.NoError(t, err)
	sig := object.Signature{Name: "x", Email: "x@mgit", When: time.Unix(0, 0).UTC()}
	otherCommit := shared.NewEncodedObject()
	require.NoError(t, (&object.Commit{Author: sig, Committer: sig, Message: "other", TreeHash: otherTreeHash}).Encode(otherCommit))
	otherCH, err := shared.SetEncodedObject(otherCommit)
	require.NoError(t, err)
	require.NoError(t, shared.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("task/OTHER-9.9"), otherCH)))

	p, err := NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	privDir := filepath.Join(t.TempDir(), "private", ".mgit")
	_, err = p.Provision(taskID, privDir)
	require.NoError(t, err)

	priv := filesystem.NewStorage(osfs.New(privDir), cache.NewObjectLRUDefault())
	_, err = priv.EncodedObject(plumbing.BlobObject, secretHash)
	assert.ErrorIs(t, err, plumbing.ErrObjectNotFound, "another task's object must never reach the private store")
}

// TestProvision_Rejections covers fail-closed input/layout guards.
func TestProvision_Rejections(t *testing.T) {
	t.Run("empty_repo_root", func(t *testing.T) {
		_, err := NewStoreProvisioner("")
		assert.Error(t, err)
	})
	t.Run("missing_task_branch", func(t *testing.T) {
		repoRoot, _, _, _ := seededRepo(t)
		p, err := NewStoreProvisioner(repoRoot)
		require.NoError(t, err)
		_, err = p.Provision("NO-SUCH-1.1", filepath.Join(t.TempDir(), ".mgit"))
		assert.ErrorIs(t, err, model.ErrBranchNotFound)
	})
	t.Run("private_dir_preexists", func(t *testing.T) {
		repoRoot, taskID, _, _ := seededRepo(t)
		p, err := NewStoreProvisioner(repoRoot)
		require.NoError(t, err)
		priv := filepath.Join(t.TempDir(), ".mgit")
		require.NoError(t, os.MkdirAll(priv, 0o700))
		_, err = p.Provision(taskID, priv)
		assert.Error(t, err)
	})
	t.Run("no_shared_store", func(t *testing.T) {
		p, err := NewStoreProvisioner(t.TempDir()) // no .mgit
		require.NoError(t, err)
		_, err = p.Provision("MGIT-1.1", filepath.Join(t.TempDir(), ".mgit"))
		assert.ErrorIs(t, err, model.ErrStorageError)
	})
}
