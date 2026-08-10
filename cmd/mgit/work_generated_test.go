package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-80: `mgit work` writes agent scaffolding into the worktree, so it must
// also RECORD that provenance under the worktree's own .mgit/ — otherwise a
// bulk stage sweeps mgit's files into the task branch and into the patch that
// lands in the user's repository.

// TestWorkSetup_OpenPosture_RecordsGeneratedClaudeMd proves the honest-open
// posture records exactly the one file it generates. Refs: MGIT-80, MGIT-47
func TestWorkSetup_OpenPosture_RecordsGeneratedClaudeMd(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-80.1"}, nil)
	require.NoError(t, err)

	got, rerr := gitstore.ReadGeneratedPaths(path)
	require.NoError(t, rerr)
	assert.Equal(t, []string{"CLAUDE.md"}, got,
		"the open posture generates only the CLAUDE.md block, and records it")
}

// TestWorkSetup_ContainedPosture_RecordsEveryGeneratedFile proves the full
// routing wiring — settings, Codex directive, Cursor rule, .envrc — is recorded
// too, so none of it can be bulk staged. Refs: MGIT-80, MGIT-11.11.1, MGIT-11.11.3
func TestWorkSetup_ContainedPosture_RecordsEveryGeneratedFile(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	_, _, err := runWorkSetup(t, adder,
		workOptions{Path: path, TaskID: "MGIT-80.2", LaunchSandbox: true}, nil)
	require.NoError(t, err)

	got, rerr := gitstore.ReadGeneratedPaths(path)
	require.NoError(t, rerr)
	assert.ElementsMatch(t, []string{
		"CLAUDE.md", ".claude/settings.json", "AGENTS.md",
		".cursor/rules/mgit-sandbox.mdc", ".envrc",
	}, got, "every generated agent file is recorded as mgit's own")

	// Every recorded path really is on disk — the record is a claim about what
	// was written, not a wish list.
	for _, rel := range got {
		_, statErr := os.Stat(filepath.Join(path, filepath.FromSlash(rel)))
		assert.NoError(t, statErr, "recorded generated path %s must exist", rel)
	}
}

// TestWorkSetup_ReRun_ManifestStaysDeduplicated proves re-running `mgit work`
// on the same worktree (idempotent re-wiring) does not accumulate duplicate
// manifest entries. Refs: MGIT-80, MGIT-34
func TestWorkSetup_ReRun_ManifestStaysDeduplicated(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")
	opts := workOptions{Path: path, TaskID: "MGIT-80.3"}
	_, _, err := runWorkSetup(t, adder, opts, nil)
	require.NoError(t, err)
	_, _, err = runWorkSetup(t, adder, opts, nil)
	require.NoError(t, err)

	got, rerr := gitstore.ReadGeneratedPaths(path)
	require.NoError(t, rerr)
	assert.Equal(t, []string{"CLAUDE.md"}, got, "re-wiring must not duplicate manifest entries")
}

// TestWorkSetup_RecordFailure_WarnsLoudlyWithConsequence proves a failure to
// record the provenance is surfaced with the consequence spelled out (the
// scaffolding may be swept into a commit) rather than silently ignored — the
// exclusion is the whole fix, so its absence must not be invisible.
// Refs: MGIT-80
func TestWorkSetup_RecordFailure_WarnsLoudlyWithConsequence(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	path := filepath.Join(t.TempDir(), "wt")

	// A file where the worktree's .mgit/ directory must go makes the manifest
	// write fail without disturbing anything else.
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, ".mgit"), []byte("not a dir\n"), 0o600))

	out, _, err := runWorkSetup(t, adder, workOptions{Path: path, TaskID: "MGIT-80.4"}, nil)
	require.NoError(t, err, "a manifest failure must not abort an otherwise-created worktree")
	assert.Contains(t, out, "mgit add", "the warning must name the remedy")
	assert.Contains(t, out, "generated", "the warning must say what was not recorded")
}
