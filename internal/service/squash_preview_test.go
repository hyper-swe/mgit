package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// PreviewGitPatch is the read-only renderer behind `mgit export --format git`.
// MGIT-112: the export used to render from model.FileDiffs, which only the
// non-dry-run squash populated, so it emitted a syntactically valid patch with
// no hunks that `git apply --allow-empty` accepted and applied to nothing.
// Refs: MGIT-112, MGIT-33, MGIT-77, FR-7

// commitFile writes a file, stages it and commits it for the given task.
func commitFile(t *testing.T, env *testEnv, taskID, path, content string) {
	t.Helper()
	ctx := context.Background()
	full := filepath.Join(env.repo.Root(), path)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	require.NoError(t, env.wt.Add(ctx, path))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: taskID, AgentID: "preview-test", Message: "write " + path,
	})
	require.NoError(t, err)
}

// removeFile deletes a file, stages the removal and commits it.
func removeFile(t *testing.T, env *testEnv, taskID, path string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, os.Remove(filepath.Join(env.repo.Root(), path)))
	require.NoError(t, env.wt.Add(ctx, path))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: taskID, AgentID: "preview-test", Message: "remove " + path,
	})
	require.NoError(t, err)
}

// TestSquashService_PreviewGitPatch_RendersRealHunks_WithoutMutatingState is
// the core MGIT-112 assertion at the service layer: the preview carries real
// diff content, and produces it without creating a squash commit, indexing one,
// or moving the task branch. Refs: MGIT-112, FR-7, FR-12
func TestSquashService_PreviewGitPatch_RendersRealHunks_WithoutMutatingState(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	const taskID = "MGIT-112"

	commitFile(t, env, taskID, "feature.go", "package feature\n")
	commitFile(t, env, taskID, "feature.go", "package feature\n\nfunc F() {}\n")

	before, err := env.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)

	preview, err := env.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	require.NotNil(t, preview)
	assert.False(t, preview.Empty, "the task has a real net change")

	assert.Contains(t, preview.Patch, "diff --git a/feature.go b/feature.go",
		"the preview must carry a real file diff, not just an mbox header")
	assert.Contains(t, preview.Patch, "@@ -0,0 +1,3 @@", "a real unified hunk header")
	assert.Contains(t, preview.Patch, "+func F() {}", "the net added content")
	assert.Contains(t, preview.Patch, "--- /dev/null",
		"an added file uses /dev/null, which is what makes it git-apply-correct")
	assert.True(t, PatchHasHunks(preview.Patch))

	// The read contract: nothing indexed, nothing branched. Refs: FR-12
	after, err := env.idx.GetTaskCommits(ctx, taskID)
	require.NoError(t, err)
	assert.Equal(t, len(before), len(after),
		"a preview must not append an indexed squash commit")
	_, err = env.bs.GetBranch(ctx, "task/"+taskID)
	assert.Error(t, err, "a preview must not create the task branch a real squash would")
}

// TestSquashService_PreviewGitPatch_MatchesGitFormatPatch_OnHunks proves the
// read path and the real squash path describe the same change. They share
// BuildSquashTree and go-git's patch encoder, so this holds by construction —
// the test pins it so a future divergence is caught. Refs: MGIT-112, FR-7
func TestSquashService_PreviewGitPatch_MatchesGitFormatPatch_OnHunks(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	const taskID = "MGIT-112"

	// Base state under a different task, so the task forks off it.
	commitFile(t, env, "BASE-0", "base.txt", "one\ntwo\n")
	commitFile(t, env, "BASE-0", "doomed.txt", "delete me\n")

	commitFile(t, env, taskID, "base.txt", "one\nTWO\nthree\n")
	commitFile(t, env, taskID, "added.txt", "brand new\n")
	removeFile(t, env, taskID, "doomed.txt")

	// Preview FIRST — it must not disturb the squash that follows.
	preview, err := env.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	require.False(t, preview.Empty)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	real, err := env.squash.GitFormatPatch(ctx, squashed)
	require.NoError(t, err)

	previewHunks := hunksOf(preview.Patch)
	require.NotEmpty(t, previewHunks, "the preview produced NO hunks (MGIT-112)")
	assert.Equal(t, hunksOf(real), previewHunks,
		"the read-only preview and the real squash must render the same diff")

	for _, want := range []string{
		"diff --git a/base.txt b/base.txt", "-two", "+TWO", "+three",
		"diff --git a/added.txt b/added.txt", "+brand new",
		"diff --git a/doomed.txt b/doomed.txt", "-delete me",
	} {
		assert.Contains(t, previewHunks, want)
	}
}

// hunksOf returns a patch's diff body, from the first "diff --git" header to
// the mbox trailer — the part a header-only patch does not have.
func hunksOf(patch string) string {
	i := strings.Index(patch, "diff --git ")
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(patch[i:], "-- \nmgit\n")
}

// TestSquashService_PreviewGitPatch_EmptyNetChange_ReportsEmptyNotAPatch covers
// the commit-and-revert case from the upstream report: the net change really is
// nothing. The preview must SAY so and hand back no patch, rather than a
// hunk-free mbox that applies cleanly and changes nothing. Refs: MGIT-112
func TestSquashService_PreviewGitPatch_EmptyNetChange_ReportsEmptyNotAPatch(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	const taskID = "MGIT-112"

	commitFile(t, env, taskID, "scratch.txt", "temporary\n")
	removeFile(t, env, taskID, "scratch.txt")

	preview, err := env.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: taskID})
	require.NoError(t, err, "an empty net change is a legitimate outcome, not a failure")
	require.NotNil(t, preview)
	assert.True(t, preview.Empty, "the task's commits cancel out against its base")
	assert.Empty(t, preview.Patch, "no patch at all, rather than a silently empty one")
	assert.Contains(t, preview.Message, taskID,
		"the message stays available so a caller can report WHAT canceled out")
}

// TestSquashService_PreviewGitPatch_RevertToOriginalContent_IsEmpty is the
// subtler empty case: the file survives, but its content is restored, so the
// net tree equals the base tree even though every micro-commit touched it.
// Refs: MGIT-112
func TestSquashService_PreviewGitPatch_RevertToOriginalContent_IsEmpty(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	const taskID = "MGIT-112"

	commitFile(t, env, "BASE-0", "cfg.txt", "original\n")
	commitFile(t, env, taskID, "cfg.txt", "experiment\n")
	commitFile(t, env, taskID, "cfg.txt", "original\n")

	preview, err := env.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: taskID})
	require.NoError(t, err)
	assert.True(t, preview.Empty,
		"restoring the base content leaves no net change to export")
	assert.Empty(t, preview.Patch)
}

// TestSquashService_PreviewGitPatch_UnknownTask_Errors proves an uncomputable
// diff fails loudly instead of yielding an applyable empty patch. Refs: MGIT-112
func TestSquashService_PreviewGitPatch_UnknownTask_Errors(t *testing.T) {
	env := setupTestEnv(t)
	preview, err := env.squash.PreviewGitPatch(context.Background(),
		SquashRequest{TaskID: "NO-SUCH-TASK"})
	require.ErrorIs(t, err, model.ErrTaskNotFound)
	assert.Nil(t, preview)
}

// TestAssertPatchCarriesHunks guards the invariant directly: a patch with no
// hunks for a task whose net change is NOT empty is an internal inconsistency
// and must surface as an error. A patch that applies cleanly and changes
// nothing is worse than a failure, because the caller has already moved on.
// Refs: MGIT-112, MGIT-77
func TestAssertPatchCarriesHunks(t *testing.T) {
	const headerOnly = "From  Mon Jan 2 15:04:05 2006\n" +
		"Subject: [PATCH] [squashed] nothing here\n\n---\n-- \nmgit\n"

	tests := []struct {
		name    string
		patch   string
		wantErr bool
	}{
		{
			name:    "header_only_patch_is_refused",
			patch:   headerOnly,
			wantErr: true,
		},
		{
			name:    "empty_string_is_refused",
			patch:   "",
			wantErr: true,
		},
		{
			name:  "patch_with_hunks_passes",
			patch: headerOnly + "diff --git a/f.txt b/f.txt\n@@ -0,0 +1 @@\n+hi\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertPatchCarriesHunks("MGIT-112", tt.patch)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, model.ErrVerificationFailed)
			assert.Contains(t, err.Error(), "MGIT-112")
			assert.Contains(t, err.Error(), "change nothing",
				"the error must name the failure mode, not just fail")
		})
	}
}
