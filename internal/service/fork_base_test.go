package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// With no readable git commit there is no committed tree to fork from: the
// caller keeps the mgit base, and nothing is created. Refs: MGIT-283
func TestCommittedForkBase_NoGitCommit_NotOK(t *testing.T) {
	env := setupTestEnv(t)
	for _, readErr := range []error{gitref.ErrNoGit, gitref.ErrDetachedOrUnborn} {
		svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
			withCommittedFilesReader(func(string) ([]gitref.CommittedFile, string, error) { return nil, "", readErr })
		base, uncommitted, ok, err := svc.CommittedForkBase(context.Background())
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, base)
		assert.Empty(t, uncommitted)
	}
}

// Any other failure to read git is loud, never a silent fallback to the
// checkout's state. Refs: MGIT-283
func TestCommittedForkBase_GitUnreadable_Errors(t *testing.T) {
	env := setupTestEnv(t)
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
		withCommittedFilesReader(func(string) ([]gitref.CommittedFile, string, error) {
			return nil, "", errors.New("corrupt pack")
		})
	_, _, _, err := svc.CommittedForkBase(context.Background())
	require.Error(t, err)
}

// The fork base's tree is exactly the committed files, and building it moves
// no branch. Refs: MGIT-283
func TestCommittedForkBase_TreeIsTheCommittedFilesAndNoBranchMoves(t *testing.T) {
	env := setupTestEnv(t)
	before, err := env.repo.Head()
	require.NoError(t, err)
	svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock()).
		withCommittedFilesReader(func(string) ([]gitref.CommittedFile, string, error) {
			return []gitref.CommittedFile{{Path: "a.go", Mode: 0o100644, Content: []byte("package a\n")}},
				"0123456789abcdef", nil
		})

	base, _, ok, err := svc.CommittedForkBase(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	after, err := env.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, before, after, "the checkout's branch does not move")
	content, err := env.cs.GetFileFromCommit(context.Background(), base, "a.go")
	require.NoError(t, err)
	assert.Equal(t, "package a\n", string(content))
	c, err := env.cs.GetCommit(context.Background(), base)
	require.NoError(t, err)
	assert.Equal(t, before, c.ParentID, "the fork base is parented on the current base")
}
