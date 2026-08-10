package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MGIT-77 acceptance criteria 4-5, end to end with the real binary: an agent
// with no knowledge beyond the CLAUDE.md that `mgit work` GENERATES must, by
// following it literally, produce commits that contain its work and a land
// patch with real diff hunks. The reported failure was the whole chain
// reporting success while carrying nothing. Refs: MGIT-77, FR-7, FR-16

// generatedCommitCommand returns the per-step checkpoint command from the
// generated "mgit working discipline" block, with its placeholder message
// replaced, ready to run verbatim.
func generatedCommitCommand(t *testing.T, claudeMd string) []string {
	t.Helper()
	start := strings.Index(claudeMd, "### mgit working discipline")
	require.Positive(t, start, "mgit work must generate the working-discipline block")

	rest := claudeMd[start:]
	for {
		i := strings.Index(rest, "`mgit commit")
		require.GreaterOrEqual(t, i, 0, "the discipline must name a concrete commit command")
		rest = rest[i+1:]
		j := strings.Index(rest, "`")
		require.Positive(t, j, "unterminated backtick in generated block")
		line := rest[:j]
		rest = rest[j:]
		if !strings.Contains(line, "-m ") {
			continue
		}
		line = strings.ReplaceAll(line, `"<what changed>"`, "step-one")
		return strings.Fields(line)[1:] // drop the "mgit" argv[0]
	}
}

func TestAgentLoop_GeneratedInstructions_ProduceALandablePatch(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()

	mustMgit(t, bin, repoDir, "init")
	commitFile(t, bin, repoDir, "MGIT-77", "seed.txt", "seed\n")

	// A fresh mgit worktree bound to the task, wired exactly as `mgit work` does.
	const task = "MGIT-77.E2E"
	wt := filepath.Join(t.TempDir(), "wt")
	mustMgit(t, bin, repoDir, "work", wt, "--task-id", task)

	claudeMd, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md")) //nolint:gosec // test-owned temp path
	require.NoError(t, err, "mgit work must generate a CLAUDE.md for the agent")

	// The agent does work, then runs the generated command verbatim. No other
	// knowledge: no `mgit add` unless the instructions said so.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feature.go"),
		[]byte("package feature\n\nfunc Answer() int { return 42 }\n"), 0o600))

	args := generatedCommitCommand(t, string(claudeMd))
	out, err := runMgit(t, bin, wt, args...)
	require.NoError(t, err, "generated instruction `mgit %s` failed: %s", strings.Join(args, " "), out)

	// The land step must carry the work.
	patch, err := runMgit(t, bin, wt, "squash", "--task-id", task, "--to-git")
	require.NoError(t, err, "squash --to-git: %s", patch)

	assert.Contains(t, patch, "diff --git a/feature.go b/feature.go",
		"the patch must carry a real file diff, not just an mbox header")
	assert.Contains(t, patch, "@@ ", "the patch must carry real unified hunks")
	assert.Contains(t, patch, "+func Answer() int { return 42 }",
		"the patch must carry the agent's actual content")
	assert.NotContains(t, patch, "contains NO diff hunks",
		"a patch with real work must not trigger the empty-patch warning")
}

// The converse: when a task's commits genuinely recorded nothing, the export
// must SAY so rather than hand over a patch `git apply` accepts silently.
// Refs: MGIT-77 (scope item 3)
func TestSquashToGit_HunkFreePatch_WarnsInsteadOfSilentlySucceeding(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()

	mustMgit(t, bin, repoDir, "init")
	mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-77.EMPTY", "-m", "records nothing", "--allow-empty")

	// runMgit merges stderr into the output, which is where the warning goes so
	// a piped patch on stdout stays clean.
	out, err := runMgit(t, bin, repoDir, "squash", "--task-id", "MGIT-77.EMPTY", "--to-git")
	require.NoError(t, err, "the export still succeeds — it warns, it does not fail: %s", out)
	assert.Contains(t, out, "contains NO diff hunks",
		"exporting a hunk-free patch must warn; landing it changes nothing")
	assert.Contains(t, out, "mgit add", "the warning must name how work gets recorded")
}
