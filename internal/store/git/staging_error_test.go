package git

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaging_CorruptStagingFile_SurfacesError(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	// Corrupt the staging file with non-JSON content.
	require.NoError(t, os.WriteFile(repo.stagingPath(), []byte("{not json"), 0o600))

	_, err := repo.stagedPaths()
	assert.Error(t, err, "corrupt staging must surface a decode error")

	_, statusErr := ws.Status(ctx)
	assert.Error(t, statusErr, "Status must propagate a corrupt-staging error")
}

func TestCreateCommit_StagedFileUnreadable_DeletesFromTree(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)

	// Commit a file, then stage its path but remove it from disk: the commit
	// must treat the missing staged path as a deletion.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "del.go"), []byte("v1\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "del.go"))
	hash1, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-7.1"))
	require.NoError(t, err)
	_, err = cs.GetFileFromCommit(ctx, hash1, "del.go")
	require.NoError(t, err)

	require.NoError(t, ws.Add(ctx, "del.go")) // stage the path again...
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "del.go")))
	hash2, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-7.2"))
	require.NoError(t, err)

	_, err = cs.GetFileFromCommit(ctx, hash2, "del.go")
	assert.Error(t, err, "staged-but-missing path must be deleted from the tree")
}

func TestCommitFromObjectData_RoundTrip(t *testing.T) {
	repo := initTestRepo(t)

	// Build a commit object via plumbing, encode it, then decode via the land
	// boundary helper and confirm the identity-bearing fields are derived.
	tree, err := emptyTree(repo.repo.Storer)
	require.NoError(t, err)
	sig := object.Signature{Name: "lander", Email: "lander@mgit", When: repo.Now()}
	commit := &object.Commit{Author: sig, Committer: sig, Message: "landed", TreeHash: tree}

	obj := repo.repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(obj))
	reader, err := obj.Reader()
	require.NoError(t, err)
	defer reader.Close() //nolint:errcheck // test cleanup
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)

	got, err := CommitFromObjectData(raw)
	require.NoError(t, err)
	assert.Equal(t, "landed", got.Message)
	assert.Equal(t, "lander", got.AgentID)
	assert.Equal(t, tree.String(), got.TreeHash)
}
