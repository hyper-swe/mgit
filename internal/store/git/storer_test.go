package git

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncingStorer_SetReference_CallsSync(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	head, err := repo.Head()
	require.NoError(t, err)

	ref := plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("test-sync"),
		plumbing.NewHash(head),
	)

	err = ss.SetReference(ref)
	assert.NoError(t, err, "SetReference with sync must succeed")
}

func TestSyncingStorer_DelegatesReadOps(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	// Read HEAD ref through syncing storer
	ref, err := ss.Reference(plumbing.HEAD)
	require.NoError(t, err)
	assert.NotNil(t, ref, "must delegate Reference() reads")
}

func TestSyncingStorer_SyncObjectsDir(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSyncingStorer(repo.repo.Storer, repo.MgitDir())

	err := ss.SyncObjectsDir()
	assert.NoError(t, err, "SyncObjectsDir must succeed")
}

func TestSyncingStorer_SyncObjectsDir_NonExistentDir(t *testing.T) {
	repo := initTestRepo(t)
	// Use a path where objects/ doesn't exist
	ss := NewSyncingStorer(repo.repo.Storer, t.TempDir())

	err := ss.SyncObjectsDir()
	assert.NoError(t, err, "SyncObjectsDir on non-existent dir should be no-op")
}
