package git

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
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

// TestCommitStore_GetCommit_AbbreviatedHash is the regression for MGIT-18:
// `mgit log` prints 8-char abbreviated hashes, but `mgit show <abbrev>` used
// to fail because GetCommit did an exact 40-char key match. GetCommit must
// resolve an unambiguous abbreviated prefix, like git.
// Refs: MGIT-18, FR-3, FR-8.7
func TestCommitStore_GetCommit_AbbreviatedHash(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	original := makeTestModelCommit(t, "MGIT-3.1")
	hash, err := cs.CreateCommit(ctx, original)
	require.NoError(t, err)

	// The 8-char prefix `mgit log` would print must resolve to the full commit.
	abbrev := hash[:8]
	retrieved, err := cs.GetCommit(ctx, abbrev)
	require.NoError(t, err, "abbreviated prefix must resolve like the full hash")
	assert.Equal(t, hash, retrieved.CommitID)
}

// TestCommitStore_GetCommit_AbbreviatedHash_NotFound verifies a prefix that
// matches no commit returns ErrCommitNotFound. Refs: MGIT-18
func TestCommitStore_GetCommit_AbbreviatedHash_NotFound(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	_, err := cs.GetCommit(ctx, "deadbeef")
	assert.ErrorIs(t, err, model.ErrCommitNotFound)
}

// TestMatchHashPrefix covers git's prefix-resolution rule: a unique prefix
// resolves to its one commit, a shared prefix is reported ambiguous (never a
// silent wrong resolution), and a prefix matching nothing is not found.
// Refs: MGIT-18
func TestMatchHashPrefix(t *testing.T) {
	candidates := []string{
		"abc123def456",
		"abc999000111",
		"ffff00001111",
	}
	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr error
	}{
		{name: "unique_prefix", ref: "ffff", want: "ffff00001111"},
		{name: "full_unique", ref: "abc123def456", want: "abc123def456"},
		{name: "ambiguous_prefix", ref: "abc", wantErr: model.ErrAmbiguousHash},
		{name: "no_match", ref: "dead", wantErr: model.ErrCommitNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := matchHashPrefix(candidates, tt.ref)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestCommitStore_GetCommit_PopulatesProvenance is the regression for MGIT-19:
// a task-tagged commit read back must surface its task_id and content_hash
// (not blank). task_id is derived from the [MGIT:TASK_ID] message prefix.
// Refs: MGIT-19, FR-3, ADR-002
func TestCommitStore_GetCommit_PopulatesProvenance(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	original := makeTestModelCommit(t, "MGIT-3.2")
	hash, err := cs.CreateCommit(ctx, original)
	require.NoError(t, err)

	retrieved, err := cs.GetCommit(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-3.2", retrieved.TaskID.String(), "task_id must be derived from the message prefix")
	assert.Equal(t, model.CommitTypeNormal, retrieved.CommitType, "commit type must default to normal")
}

func TestCommitStore_DeleteCommitRejectsAppendOnly(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	err := cs.DeleteCommit(ctx, "anyhash")
	assert.ErrorIs(t, err, model.ErrAppendOnlyViolation,
		"DeleteCommit must always return ErrAppendOnlyViolation")
}
