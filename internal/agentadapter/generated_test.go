package agentadapter

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filesUnder returns the worktree-relative slash paths of every file under
// root, skipping the .mgit/ store (mgit's own dir, already excluded from
// staging and therefore not part of the scaffolding problem).
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".mgit" || strings.HasPrefix(rel, ".mgit/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			out = append(out, rel)
		}
		return nil
	}))
	return out
}

// TestGeneratedWorktreeFiles_ContainedPosture_MatchesWhatTheAdaptersWrite is
// the guard that keeps the MGIT-80 exclusion honest: the declared list must be
// EXACTLY the set of files the adapter writers actually produce, so adding a
// new generated file without declaring it fails here instead of silently
// landing in a user's repository. Refs: MGIT-80, MGIT-11.11.1, MGIT-11.11.3
func TestGeneratedWorktreeFiles_ContainedPosture_MatchesWhatTheAdaptersWrite(t *testing.T) {
	wt := t.TempDir()

	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "none"}))
	require.NoError(t, WriteClaudeSettings(wt, "mgit sandbox claude-hook"))
	require.NoError(t, InstallCooperativeAdapters(wt, "mgit"))

	assert.ElementsMatch(t, filesUnder(t, wt), GeneratedWorktreeFiles(true),
		"GeneratedWorktreeFiles(contained) must list exactly the files the adapters write")
}

// TestGeneratedWorktreeFiles_OpenPosture_MatchesWhatTheAdaptersWrite pins the
// honest-open posture (MGIT-47): no routing wiring is installed, so the only
// generated file is the CLAUDE.md block. Refs: MGIT-80, MGIT-47
func TestGeneratedWorktreeFiles_OpenPosture_MatchesWhatTheAdaptersWrite(t *testing.T) {
	wt := t.TempDir()

	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{
		WorktreePath: wt, NetworkMode: "none", Containment: ContainmentOpen,
	}))

	assert.ElementsMatch(t, filesUnder(t, wt), GeneratedWorktreeFiles(false),
		"in the open posture only the CLAUDE.md block is generated")
}

// TestGeneratedWorktreeFiles_EveryPath_IsWorktreeRelative proves no declared
// path is absolute or escaping — the store refuses such entries, which would
// mean the exclusion silently failed. Refs: MGIT-80
func TestGeneratedWorktreeFiles_EveryPath_IsWorktreeRelative(t *testing.T) {
	for _, contained := range []bool{false, true} {
		for _, p := range GeneratedWorktreeFiles(contained) {
			assert.False(t, filepath.IsAbs(p), "generated path %q must be worktree-relative", p)
			assert.NotContains(t, p, "..", "generated path %q must not escape the worktree", p)
			assert.Equal(t, filepath.ToSlash(p), p, "generated path %q must use slash separators", p)
		}
	}
}

// TestExistingGeneratedFiles_OnlyReportsWhatIsOnDisk proves the recorder is
// precise: a declared file the writers failed to produce is not claimed as
// mgit-generated, so a user's own later file of that name is still bulk
// staged. Refs: MGIT-80
func TestExistingGeneratedFiles_OnlyReportsWhatIsOnDisk(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, UpsertClaudeMd(wt, SandboxEnv{WorktreePath: wt, NetworkMode: "none"}))

	got := ExistingGeneratedFiles(wt, true)
	assert.Equal(t, []string{"CLAUDE.md"}, got,
		"only files actually written count as generated")

	require.NoError(t, os.WriteFile(filepath.Join(wt, ".envrc"), []byte("x\n"), 0o600))
	assert.Contains(t, ExistingGeneratedFiles(wt, true), ".envrc")
}
