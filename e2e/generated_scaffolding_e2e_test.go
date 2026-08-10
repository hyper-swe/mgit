package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MGIT-80 end to end with the real binary: `mgit work` writes mgit's OWN agent
// scaffolding into the worktree (the generated CLAUDE.md block,
// .claude/settings.json and, under containment, AGENTS.md / .envrc / the Cursor
// rule). With MGIT-77 prescribing `mgit commit -a` as THE agent loop, none of
// that may reach the task branch or the patch that lands in the user's repo —
// its content describes one worktree's task binding and this host's sandbox
// availability, which is wrong at the destination.

// scaffoldingPaths are the files `mgit work` may generate into a worktree; none
// of them may appear in a landed patch unless the user staged one by name.
var scaffoldingPaths = []string{
	"CLAUDE.md", ".claude/settings.json", "AGENTS.md",
	".cursor/rules/mgit-sandbox.mdc", ".envrc",
}

// assertNoScaffoldingInPatch fails with the offending path when a squash patch
// carries any mgit-generated file.
func assertNoScaffoldingInPatch(t *testing.T, patch string) {
	t.Helper()
	for _, p := range scaffoldingPaths {
		assert.NotContains(t, patch, "diff --git a/"+p+" b/"+p,
			"mgit's generated %s must never land in the user's repository", p)
	}
	assert.NotContains(t, patch, "BEGIN mgit-sandbox",
		"no mgit-generated block may appear in the patch body")
}

// TestGeneratedScaffolding_NeverLandsInThePatch covers BOTH shapes of the bug:
// a fresh project where CLAUDE.md is a brand-new untracked file, and a project
// that already tracks CLAUDE.md and .claude/settings.json, where mgit's block
// is an EDIT to a tracked file. Refs: MGIT-80, MGIT-77
func TestGeneratedScaffolding_NeverLandsInThePatch(t *testing.T) {
	tests := []struct {
		name string
		// seeded files are committed into the base BEFORE the worktree exists,
		// so mgit's later block is a modification of a tracked file.
		seeded  map[string]string
		sandbox bool
	}{
		{
			name:   "fresh_project_scaffolding_is_untracked",
			seeded: map[string]string{"main.go": "package main\n"},
		},
		{
			name: "project_already_tracks_claude_md",
			seeded: map[string]string{
				"main.go":               "package main\n",
				"CLAUDE.md":             "# Project directives\n\nBe precise.\n",
				".claude/settings.json": "{\n  \"model\": \"opus\"\n}\n",
			},
		},
		{
			name:    "contained_posture_writes_the_full_wiring",
			seeded:  map[string]string{"main.go": "package main\n"},
			sandbox: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := buildMgitBinary(t)
			repoDir := t.TempDir()
			mustMgit(t, bin, repoDir, "init")
			for rel, content := range tt.seeded {
				writeWorktreeFile(t, repoDir, rel, content)
				mustMgit(t, bin, repoDir, "add", rel)
			}
			mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-80.SEED", "-m", "seed")

			const task = "MGIT-80.E2E"
			wt := filepath.Join(t.TempDir(), "wt")
			args := []string{"work", wt, "--task-id", task}
			if tt.sandbox {
				// --sandbox on a host with no backend: the wiring is still written
				// (fail-closed posture), only the launch leg degrades.
				args = append(args, "--sandbox")
			}
			_, _ = runMgit(t, bin, repoDir, args...)
			require.FileExists(t, filepath.Join(wt, "CLAUDE.md"),
				"mgit work must have generated the scaffolding this test is about")

			// The agent does its work and runs the prescribed one-command loop.
			writeWorktreeFile(t, wt, "feature.go", "package feature\n\nfunc Answer() int { return 42 }\n")
			out, err := runMgit(t, bin, wt, "commit", "-a", "-m", "add feature")
			require.NoError(t, err, "mgit commit -a: %s", out)

			patch, err := runMgit(t, bin, wt, "squash", "--task-id", task, "--to-git")
			require.NoError(t, err, "squash --to-git: %s", patch)

			assert.Contains(t, patch, "diff --git a/feature.go b/feature.go",
				"the agent's own work must still land")
			assert.Contains(t, patch, "+func Answer() int { return 42 }")
			assertNoScaffoldingInPatch(t, patch)
		})
	}
}

// TestGeneratedScaffolding_ExplicitlyStagedClaudeMd_StillLands pins the decided
// escape hatch end to end: a user who deliberately edits CLAUDE.md and names it
// to `mgit add` gets their edit committed and landed, while mgit's own
// generated block stays out. Refs: MGIT-80
func TestGeneratedScaffolding_ExplicitlyStagedClaudeMd_StillLands(t *testing.T) {
	bin := buildMgitBinary(t)
	repoDir := t.TempDir()
	mustMgit(t, bin, repoDir, "init")
	writeWorktreeFile(t, repoDir, "CLAUDE.md", "# Project directives\n\nBe precise.\n")
	mustMgit(t, bin, repoDir, "add", "CLAUDE.md")
	mustMgit(t, bin, repoDir, "commit", "--task-id", "MGIT-80.SEED2", "-m", "seed")

	const task = "MGIT-80.EXPLICIT"
	wt := filepath.Join(t.TempDir(), "wt")
	_, _ = runMgit(t, bin, repoDir, "work", wt, "--task-id", task)

	// The user deliberately adds a directive of their own, ABOVE mgit's block.
	current, err := os.ReadFile(filepath.Join(wt, "CLAUDE.md")) //nolint:gosec // test temp path
	require.NoError(t, err)
	updated := strings.Replace(string(current), "Be precise.\n", "Be precise.\nAlways run the linter.\n", 1)
	require.NotEqual(t, string(current), updated, "the seeded directive must be present to edit")
	writeWorktreeFile(t, wt, "CLAUDE.md", updated)

	addOut := mustMgit(t, bin, wt, "add", "CLAUDE.md")
	assert.Contains(t, addOut, "generated by mgit for this worktree",
		"the escape hatch is honored, but never silent about what else it commits")

	out, err := runMgit(t, bin, wt, "commit", "-a", "-m", "add a project directive")
	require.NoError(t, err, "commit: %s", out)

	patch, err := runMgit(t, bin, wt, "squash", "--task-id", task, "--to-git")
	require.NoError(t, err, "squash --to-git: %s", patch)
	assert.Contains(t, patch, "diff --git a/CLAUDE.md b/CLAUDE.md",
		"an explicitly staged CLAUDE.md must still land")
	assert.Contains(t, patch, "+Always run the linter.",
		"the user's own directive is the content that lands")
}

// writeWorktreeFile writes a file under root, creating parent directories.
func writeWorktreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600)) //nolint:gosec // test-owned temp path
}
