package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestRepository_CurrentBranch_OnMain(t *testing.T) {
	repo := initTestRepo(t)

	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	assert.Equal(t, "main", branch, "after init HEAD should point to main")
}

func TestRepository_CurrentBranch_AfterSwitch(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	bs := NewBranchStore(repo)

	head, err := repo.Head()
	require.NoError(t, err)

	tid, _ := model.ParseTaskID("MGIT-1.1")
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature-x", HeadCommit: head, TaskID: tid}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature-x"))

	branch, err := repo.CurrentBranch()
	require.NoError(t, err)
	assert.Equal(t, "feature-x", branch)
}

func TestRepository_CurrentBranch_DetachedHEAD(t *testing.T) {
	repo := initTestRepo(t)

	// Detach HEAD by pointing it directly at a hash (not a branch)
	ref, err := repo.repo.Head()
	require.NoError(t, err)

	// Set HEAD to a non-symbolic (detached) reference
	detachedRef := plumbing.NewHashReference(plumbing.HEAD, ref.Hash())
	require.NoError(t, repo.repo.Storer.SetReference(detachedRef))

	_, err = repo.CurrentBranch()
	assert.Error(t, err, "detached HEAD should error")
	assert.Contains(t, err.Error(), "detached", "error must mention detached HEAD")
}

func TestRepository_Open_NotADirectory(t *testing.T) {
	tmpDir := t.TempDir()
	// Create .mgit as a file, not a directory
	err := os.WriteFile(filepath.Join(tmpDir, ".mgit"), []byte("not a dir"), 0o600)
	require.NoError(t, err)

	_, err = Open(tmpDir, fixedClock())
	assert.Error(t, err, "Open should fail when .mgit is a file")
	assert.ErrorIs(t, err, model.ErrStorageError)
}
