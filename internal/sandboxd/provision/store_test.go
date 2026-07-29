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
	t.Run("unknown_task_seeds_from_head_rather_than_failing", func(t *testing.T) {
		// SEMANTICS CHANGE (MGIT-62): a task with no task branch is no longer
		// a rejection. Task branches come from squash, so "no task branch"
		// is the NORMAL state of a task that has only been committed — or
		// never worked on at all. Both mean "start from the project's current
		// state", which is what a user asking for a sandbox expects.
		repoRoot, _, _, _ := seededRepo(t)
		p, err := NewStoreProvisioner(repoRoot)
		require.NoError(t, err)
		store, err := p.Provision("NO-SUCH-1.1", filepath.Join(t.TempDir(), ".mgit"))
		require.NoError(t, err)

		priv := filesystem.NewStorage(osfs.New(store.Dir), cache.NewObjectLRUDefault())
		_, err = priv.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName("NO-SUCH-1.1")))
		assert.NoError(t, err, "the private store names the task branch however it was seeded")
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

// MGIT-62 — FIRST-USE. The product's headline order is work -> commit ->
// `mgit run --sandbox`; squash is an integration step that comes later. But
// task branches are created by SQUASH, not by commit, so a task that has been
// committed and never squashed had no task/<ID> to seed from and every
// sandbox launch failed "branch not found". These pin the semantics chosen to
// fix it: the task branch is used WHEN IT EXISTS (the squash-first path is
// untouched), otherwise the shared store's HEAD is the base — "sandbox the
// work I have now" — and the private store still exposes the task branch, so
// land's contract is unchanged either way. Refs: MGIT-62, SEC-03, FR-17.5

// repoWithHeadOnly builds a project whose HEAD carries a commit but which has
// NO task branch — exactly the state after `mgit init` + `mgit commit`.
func repoWithHeadOnly(t *testing.T) (repoRoot, headHash, blobContent string) {
	t.Helper()
	repoRoot = t.TempDir()
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	blobContent = "committed but never squashed"
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
		{Name: "work.txt", Mode: 0o100644, Hash: blobHash},
	}}).Encode(treeObj))
	treeHash, err := shared.SetEncodedObject(treeObj)
	require.NoError(t, err)

	sig := object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()}
	commitObj := shared.NewEncodedObject()
	require.NoError(t, (&object.Commit{Author: sig, Committer: sig, Message: "work", TreeHash: treeHash}).Encode(commitObj))
	ch, err := shared.SetEncodedObject(commitObj)
	require.NoError(t, err)

	// HEAD -> refs/heads/main -> the commit. No task branch anywhere.
	main := plumbing.NewBranchReferenceName("main")
	require.NoError(t, shared.SetReference(plumbing.NewHashReference(main, ch)))
	require.NoError(t, shared.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, main)))
	return repoRoot, ch.String(), blobContent
}

func TestProvision_NeverSquashedTask_SeedsFromHead(t *testing.T) {
	repoRoot, headHash, blobContent := repoWithHeadOnly(t)
	p, err := NewStoreProvisioner(repoRoot)
	require.NoError(t, err)

	privDir := filepath.Join(t.TempDir(), "private-store")
	store, err := p.Provision("MGIT-62", privDir)
	require.NoError(t, err,
		"a task committed but never squashed must still launch a sandbox — this is the first thing a new user does")

	priv := filesystem.NewStorage(osfs.New(store.Dir), cache.NewObjectLRUDefault())

	// The private store still exposes the TASK branch, so the guest commits
	// on task/<ID> and land's contract is identical to the squash-first path.
	ref, err := priv.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName("MGIT-62")))
	require.NoError(t, err, "the private store must carry the task branch regardless of how it was seeded")
	assert.Equal(t, headHash, ref.Hash().String(), "the base must be the work the user actually has (HEAD)")

	head, err := priv.Reference(plumbing.HEAD)
	require.NoError(t, err)
	assert.Equal(t, model.TaskBranchName("MGIT-62"), head.Target().Short())

	// And the user's work is genuinely reachable in the guest's store.
	commit, err := object.GetCommit(priv, ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	f, err := tree.File("work.txt")
	require.NoError(t, err)
	got, err := f.Contents()
	require.NoError(t, err)
	assert.Equal(t, blobContent, got)
}

func TestProvision_TaskBranchWins_WhenBothExist(t *testing.T) {
	// The squash-first path must be UNCHANGED: when task/<ID> exists it is
	// the base, even though HEAD also points somewhere.
	repoRoot, taskID, baseHash, _ := seededRepo(t)

	// Give the repo a HEAD pointing elsewhere, so "seed from HEAD" would be
	// observably wrong if it took precedence.
	shared := filesystem.NewStorage(osfs.New(filepath.Join(repoRoot, ".mgit")), cache.NewObjectLRUDefault())
	other := plumbing.NewBranchReferenceName("main")
	require.NoError(t, shared.SetReference(plumbing.NewHashReference(other, plumbing.NewHash(baseHash))))
	require.NoError(t, shared.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, other)))

	p, err := NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	store, err := p.Provision(taskID, filepath.Join(t.TempDir(), "private-store"))
	require.NoError(t, err)

	priv := filesystem.NewStorage(osfs.New(store.Dir), cache.NewObjectLRUDefault())
	ref, err := priv.Reference(plumbing.NewBranchReferenceName(model.TaskBranchName(taskID)))
	require.NoError(t, err)
	assert.Equal(t, baseHash, ref.Hash().String(), "the task branch must remain the base when it exists")
}

func TestProvision_EmptyRepoNoHead_StillFailsClosed(t *testing.T) {
	// A store with neither a task branch nor a resolvable HEAD has nothing
	// to seed from; that must remain an error rather than silently producing
	// an empty store the guest would commit into detached from the project.
	repoRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".mgit"), 0o700))

	p, err := NewStoreProvisioner(repoRoot)
	require.NoError(t, err)
	_, err = p.Provision("MGIT-62", filepath.Join(t.TempDir(), "private-store"))
	require.Error(t, err, "no base at all must fail closed")
	assert.ErrorIs(t, err, model.ErrBranchNotFound)
}
