package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// commitFileOn writes content to file in the repo root, stages it, and commits
// it under taskID on the current branch.
func commitFileOn(t *testing.T, env *testEnv, taskID, file, content string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), file), []byte(content), 0o600))
	require.NoError(t, env.wt.Add(ctx, file))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{TaskID: taskID, AgentID: "a", Message: "edit " + file})
	require.NoError(t, err)
}

// TestMergeService_Merge_ConflictingPaths_Error: when both the source branch and
// HEAD modify the same file with different content relative to the merge base,
// the merge is refused with ErrMergeConflict (detected before any commit). This
// is the conflict-detection feature, previously untested. Refs: FR-8.4
func TestMergeService_Merge_ConflictingPaths_Error(t *testing.T) {
	env, mergeSvc := setupMergeEnv(t)
	ctx := context.Background()

	// Base on main.
	commitFileOn(t, env, "MGIT-1", "shared.go", "package shared // base\n")

	// Feature branch diverges and changes shared.go one way.
	_, err := env.branch.CreateBranch(ctx, "MGIT-2")
	require.NoError(t, err)
	require.NoError(t, env.branch.SwitchBranch(ctx, "task/MGIT-2"))
	commitFileOn(t, env, "MGIT-2", "shared.go", "package shared // FEATURE edit\n")

	// main changes shared.go a different way.
	require.NoError(t, env.branch.SwitchBranch(ctx, "main"))
	commitFileOn(t, env, "MGIT-1", "shared.go", "package shared // MAIN edit\n")

	// Merging the divergent branch must surface a conflict, not silently win.
	_, err = mergeSvc.Merge(ctx, MergeRequest{SourceBranch: "task/MGIT-2", Strategy: MergeNoFF})
	assert.ErrorIs(t, err, model.ErrMergeConflict,
		"both sides editing shared.go differently must be a merge conflict")
}
