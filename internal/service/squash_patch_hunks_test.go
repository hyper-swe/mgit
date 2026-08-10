package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MGIT-77 scope item 3: a land step that silently lands nothing is the same
// failure one layer down. `squash --to-git` on a branch of empty commits emits
// a well-formed mbox with zero diff hunks, and `git apply` accepts it happily.
// PatchHasHunks is the predicate the CLI warns on. Refs: FR-7, MGIT-77

func TestPatchHasHunks_Classification(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  bool
	}{
		{name: "empty_string", patch: "", want: false},
		{
			name: "mbox_header_only",
			patch: "From abc Mon Jan 2 15:04:05 2006\nFrom: a <a@mgit.local>\n" +
				"Subject: [PATCH] [squashed] nothing\n\n-- \nmgit\n",
			want: false,
		},
		{
			name: "carries_a_file_diff",
			patch: "Subject: [PATCH] [squashed] real\n\ndiff --git a/f.go b/f.go\n" +
				"--- /dev/null\n+++ b/f.go\n@@ -0,0 +1 @@\n+x\n",
			want: true,
		},
		{
			name:  "diff_git_mentioned_in_the_commit_message_body_only",
			patch: "Subject: [PATCH] [squashed] explain diff --git usage\n\n-- \nmgit\n",
			want:  true, // conservative: a false "has hunks" only suppresses a warning
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PatchHasHunks(tt.patch))
		})
	}
}

func TestSquashService_GitFormatPatch_EmptyCommits_ProducesNoHunks(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "a", Message: "empty step", AllowEmpty: true,
	})
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-77"})
	require.NoError(t, err)
	patch, err := env.squash.GitFormatPatch(ctx, squashed)
	require.NoError(t, err)

	assert.NotEmpty(t, patch, "the mbox is still well formed — that is exactly why it is dangerous")
	assert.False(t, PatchHasHunks(patch),
		"a squash of empty commits carries no diff hunks and must be detectable as such")
}

func TestSquashService_GitFormatPatch_RealChange_HasHunks(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "landed.txt"), []byte("landed\n"), 0o600))
	require.NoError(t, env.wt.Add(ctx, "landed.txt"))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-77", AgentID: "a", Message: "real step",
	})
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-77"})
	require.NoError(t, err)
	patch, err := env.squash.GitFormatPatch(ctx, squashed)
	require.NoError(t, err)

	assert.True(t, PatchHasHunks(patch), "a squash of real work must report hunks")
}
