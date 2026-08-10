package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-77 scope item 4: before `mgit add` an entry rendered as "  M path" and
// after as "M   path" — a column shift was the ONLY difference between "will be
// committed" and "will be silently dropped". The default output now says which
// is which in words. The machine-readable modes must NOT change. Refs: FR-8.6, MGIT-77

func TestStatus_DefaultOutput_NamesTheStagedSplitInWords(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	// A tracked file to modify without staging.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v1\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "tracked.txt"))
	require.NoError(t, runCLI(t, "commit", "--task-id", "MGIT-77", "-m", "seed"))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v2\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("new\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("loose\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "staged.txt"))

	out, err := runCLIOut(t, "status")
	require.NoError(t, err)

	assert.Contains(t, out, "Changes to be committed", "the staged group must be named in words")
	assert.Contains(t, out, "Changes not staged for commit", "the unstaged group must be named in words")
	assert.Contains(t, out, "Untracked files", "untracked files must be named in words")
	assert.Contains(t, out, "new file:   staged.txt")
	assert.Contains(t, out, "modified:   tracked.txt")
	assert.Contains(t, out, "untracked.txt")
	assert.Contains(t, out, "mgit add", "the remedy for unrecorded work must be on screen")
	assert.Contains(t, out, "mgit commit -a", "the one-command alternative must be on screen")
}

func TestStatus_DefaultOutput_NothingStaged_SaysCommitWouldRecordNothing(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))

	out, err := runCLIOut(t, "status")
	require.NoError(t, err)

	assert.Contains(t, out, "Untracked files")
	assert.Contains(t, out, "nothing staged",
		"with nothing staged the user must be told a commit would record nothing")
}

func TestStatusLabel_EveryCode_RendersAWord(t *testing.T) {
	tests := []struct {
		name, code, want string
	}{
		{name: "added", code: gitstore.StatusAdded, want: "new file:   "},
		{name: "deleted", code: gitstore.StatusDeleted, want: "deleted:    "},
		{name: "modified", code: gitstore.StatusModified, want: "modified:   "},
		{name: "untracked_needs_no_label", code: gitstore.StatusUntracked, want: ""},
		{name: "unknown_code_falls_back_to_the_raw_character", code: "X", want: "X:          "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, statusLabel(tt.code))
		})
	}
}

// A deletion is a change, and it must read as one in both groups.
func TestPrintGroupedStatus_Deletions_ReadAsDeletions(t *testing.T) {
	var buf bytes.Buffer
	printGroupedStatus(&buf, []gitstore.FileStatus{
		{Path: "staged-delete.txt", Staging: gitstore.StatusDeleted, Worktree: gitstore.StatusUnmodified},
		{Path: "loose-delete.txt", Staging: gitstore.StatusUnmodified, Worktree: gitstore.StatusDeleted},
	})
	out := buf.String()

	assert.Contains(t, out, "Changes to be committed")
	assert.Contains(t, out, "deleted:    staged-delete.txt")
	assert.Contains(t, out, "Changes not staged for commit")
	assert.Contains(t, out, "deleted:    loose-delete.txt")
	assert.Contains(t, out, "1 staged, 1 not staged.")
}

func TestStatus_MachineReadableModes_UnchangedByTheWording(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600))
	require.NoError(t, runCLI(t, "add", "a.txt"))

	porcelain, err := runCLIOut(t, "status", "--porcelain")
	require.NoError(t, err)
	assert.Equal(t, "A  a.txt\n", porcelain, "porcelain stays a stable two-column format")
	assert.NotContains(t, porcelain, "Changes to be committed")

	short, err := runCLIOut(t, "status", "--short")
	require.NoError(t, err)
	assert.Equal(t, "A  a.txt\n", short)

	js, err := runCLIOut(t, "status", "--json")
	require.NoError(t, err)
	assert.Contains(t, js, `"staging":"A"`)
	assert.NotContains(t, js, "Changes to be committed")
}
