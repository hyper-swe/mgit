package git

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/astutic/mgit/internal/model"
)

func makeTestModelCommit(t *testing.T, taskID string) *model.Commit {
	t.Helper()
	tid, err := model.ParseTaskID(taskID)
	require.NoError(t, err)
	return &model.Commit{
		TaskID:     tid,
		AgentID:    "test-agent",
		SessionID:  "session-001",
		Message:    "[MGIT:" + taskID + "] test commit",
		CommitType: model.CommitTypeNormal,
		CreatedBy:  "test-agent",
		Branch:     "task/" + taskID,
		FileDiffs: []model.FileDiff{
			{Path: "main.go", Operation: model.DiffAdded, NewHash: "abc123"},
		},
	}
}

func TestCommitStore_CreateCommit(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	commit := makeTestModelCommit(t, "MGIT-1.2.3")
	hash, err := cs.CreateCommit(ctx, commit)
	require.NoError(t, err, "CreateCommit must succeed")
	assert.NotEmpty(t, hash, "must return commit hash")
}

func TestCommitStore_CreateCommit_ReturnsHash(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	commit := makeTestModelCommit(t, "MGIT-1.2.3")
	hash, err := cs.CreateCommit(ctx, commit)
	require.NoError(t, err)

	// SHA-1 hash should be 40 hex characters
	assert.Len(t, hash, 40, "SHA-1 hash must be 40 characters")

	// commit_id and content_hash should be populated
	assert.NotEmpty(t, commit.CommitID, "CommitID (SHA-1) must be set")
	assert.NotEmpty(t, commit.ContentHash, "ContentHash (SHA-256) must be set")
}

func TestCommitStore_CreateCommit_SetsTimestamp(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	commit := makeTestModelCommit(t, "MGIT-2.1")
	_, err := cs.CreateCommit(ctx, commit)
	require.NoError(t, err)

	expected := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, expected, commit.CreatedAt, "timestamp must come from injected clock")
}

func TestCommitStore_GetCommit(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	original := makeTestModelCommit(t, "MGIT-3.1")
	hash, err := cs.CreateCommit(ctx, original)
	require.NoError(t, err)

	retrieved, err := cs.GetCommit(ctx, hash)
	require.NoError(t, err, "GetCommit must succeed for existing commit")

	assert.Equal(t, hash, retrieved.CommitID)
	assert.Contains(t, retrieved.Message, "MGIT-3.1")
}

func TestCommitStore_GetCommit_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	_, err := cs.GetCommit(ctx, "0000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

func TestCommitStore_ListCommits(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	// Create two commits
	c1 := makeTestModelCommit(t, "MGIT-1.1")
	_, err := cs.CreateCommit(ctx, c1)
	require.NoError(t, err)

	c2 := makeTestModelCommit(t, "MGIT-1.2")
	_, err = cs.CreateCommit(ctx, c2)
	require.NoError(t, err)

	commits, err := cs.ListCommits(ctx)
	require.NoError(t, err)

	// Should have at least 2 commits + initial commit
	assert.GreaterOrEqual(t, len(commits), 2, "must return at least 2 commits")
}

func TestCommitStore_DeleteCommitRejectsAppendOnly(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	err := cs.DeleteCommit(ctx, "anyhash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation,
		"DeleteCommit must always return ErrAppendOnlyViolation")
}
