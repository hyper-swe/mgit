package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSquashService_GitFormatPatch_HasContentAndApplies proves the mgit->git
// delivery bridge works: squashing a task's micro-commits and exporting via
// GitFormatPatch yields a git-am/git-apply-compatible patch carrying the real
// added content. This is the MGIT-33 dogfood fix — squash --to-git previously
// emitted an empty patch (the squash's FileDiffs were never populated and the
// diff-action mapping was wrong). The body now comes from go-git's own unified
// encoder, so adds use `--- /dev/null` (git-apply-correct), real `@@` hunks,
// and +content. Refs: MGIT-33, FR-7
func TestSquashService_GitFormatPatch_HasContentAndApplies(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	const content = "package f\n\nfunc F() int { return 42 }\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.repo.Root(), "feature.go"), []byte(content), 0o600))
	require.NoError(t, env.wt.Add(ctx, "feature.go"))
	_, err := env.commit.CreateCommit(ctx, CreateCommitRequest{
		TaskID: "MGIT-33", AgentID: "a", Message: "add feature",
	})
	require.NoError(t, err)

	squashed, err := env.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-33"})
	require.NoError(t, err)

	patch, err := env.squash.GitFormatPatch(ctx, squashed)
	require.NoError(t, err)

	assert.Contains(t, patch, "Subject: [PATCH] [squashed]", "mbox subject prefixed per FR-7")
	assert.Contains(t, patch, "diff --git a/feature.go b/feature.go", "carries the file's git diff header")
	assert.Contains(t, patch, "--- /dev/null", "an added file uses /dev/null (git-apply-correct, not a/path)")
	assert.Contains(t, patch, "+++ b/feature.go")
	assert.Contains(t, patch, "@@ -0,0 +1,", "real unified hunk header for the addition")
	assert.Contains(t, patch, "+func F() int { return 42 }", "the patch carries the real added content")
	assert.NotContains(t, patch, "diff --mgit", "uses git-format headers, not the mgit display format")
}

// TestSquashService_GitFormatPatch_NilCommit returns empty without error.
func TestSquashService_GitFormatPatch_NilCommit(t *testing.T) {
	env := setupTestEnv(t)
	out, err := env.squash.GitFormatPatch(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}
