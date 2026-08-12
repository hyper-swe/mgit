package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-105: `mgit commit` only took -m, so any message carrying shell
// metacharacters had to survive a round trip through the shell — and the
// shell's failure modes are silent truncation and mangling, not refusal. A
// commit message is the reviewer's record of what an agent did, so these tests
// assert on the RECORDED BYTES: a substring assertion would pass on a
// truncated message, which is the exact defect being defended against.
// Refs: FR-2.9, FR-8.3, MGIT-105

// nastyMessage carries every character class that dies in a shell: backticks,
// double quotes, single quotes, a command substitution, internal blank lines
// and trailing newlines.
const nastyMessage = "fix(sandbox): quote `krun_start_enter` and $(command -v mgit)\n" +
	"\n" +
	"The body names \"double quotes\", 'single quotes', a backticked symbol\n" +
	"`krun_start_enter`, and a substitution $(echo pwned) that must never\n" +
	"reach a shell.\n" +
	"\n" +
	"\n" +
	"Refs: MGIT-105\n" +
	"\n"

// commitMessageBytes reads a commit's message from the .mgit store via the
// store layer (never by shelling out to git).
func commitMessageBytes(t *testing.T, repoRoot, commitID string) string {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	c, err := gitstore.NewCommitStore(r).GetCommit(context.Background(), commitID)
	require.NoError(t, err)
	return c.Message
}

// seedRepoForMessageFile initializes a repo with one commit so HEAD exists,
// and returns the repo root (already the working directory).
func seedRepoForMessageFile(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-105", "-m", "seed"))
	return repo
}

// TestCommit_MessageFile_NastyMessage_RoundTripsByteIdentical is the required
// test from the founder ruling: bytes in, identical bytes out of the recorded
// commit and out of what `mgit show`/`mgit log` report. Refs: MGIT-105
func TestCommit_MessageFile_NastyMessage_RoundTripsByteIdentical(t *testing.T) {
	repo := seedRepoForMessageFile(t)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte(nastyMessage), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-105", "-F", msgFile))

	head := headCommitID(t, repo)
	want := "[MGIT:MGIT-105] " + nastyMessage

	got := commitMessageBytes(t, repo, head)
	require.Equal(t, want, got,
		"the recorded message must be the file's bytes verbatim (task prefix aside)")
	require.Len(t, got, len(want), "no byte may be trimmed, added or normalized")

	// The same bytes must come back out of the surfaces a reviewer reads.
	// Decode only the message field: model.Commit's own task-ID parsing would
	// reject the repository's untagged initial commit and mask the assertion.
	type reported struct {
		Message string `json:"message"`
	}

	out, err := runCLIOut(t, "show", head, "--json")
	require.NoError(t, err)
	var shown reported
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	assert.Equal(t, want, shown.Message, "mgit show must report the recorded bytes")

	out, err = runCLIOut(t, "log", "--json")
	require.NoError(t, err)
	var logged []reported
	require.NoError(t, json.Unmarshal([]byte(out), &logged))
	require.NotEmpty(t, logged)
	assert.Equal(t, want, logged[0].Message, "mgit log must report the recorded bytes")
}

// TestCommit_MessageFileStdin_RecordsBytesVerbatim covers `-F -`, the path a
// programmatic caller uses to avoid a temp file entirely — a first-class case,
// not an afterthought. The real-process half proves the process stdin wiring an
// in-process cobra invocation cannot observe. Refs: MGIT-105
func TestCommit_MessageFileStdin_RecordsBytesVerbatim(t *testing.T) {
	t.Run("in_process", func(t *testing.T) {
		repo := seedRepoForMessageFile(t)
		require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))
		require.NoError(t, runCLIWithStdin(t, nastyMessage,
			"commit", "-a", "--task-id", "MGIT-105", "-F", "-"))

		require.Equal(t, "[MGIT:MGIT-105] "+nastyMessage,
			commitMessageBytes(t, repo, headCommitID(t, repo)),
			"stdin must be read as bytes, with no line-based mangling")
	})

	t.Run("real_process", func(t *testing.T) {
		bin := buildMgitTestBinary(t)
		repo := t.TempDir()
		require.NoError(t, runMgitTest(t, bin, repo, "init"))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
		require.NoError(t, runMgitTest(t, bin, repo, "commit", "-a", "--task-id", "MGIT-105", "-m", "seed"))

		require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))
		_, err := runMgitTestStdin(t, bin, repo, nastyMessage,
			"commit", "-a", "--task-id", "MGIT-105", "-F", "-")
		require.NoError(t, err)

		require.Equal(t, "[MGIT:MGIT-105] "+nastyMessage,
			commitMessageBytes(t, repo, headCommitID(t, repo)),
			"the compiled binary must read the message from its real stdin")
	})
}

// runCLIWithStdin executes one mgit command through the real root command with
// stdin wired to in, so `-F -` can be exercised in process. Refs: MGIT-105
func runCLIWithStdin(t *testing.T, in string, args ...string) error {
	t.Helper()
	var out bytes.Buffer
	root := rootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(in))
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		t.Logf("mgit %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return err
}

// TestCommit_MessageFlagWithFileFlag_Refused_NamesBothFlags: silently
// preferring one source would reintroduce the defect class — the caller
// believes it recorded one thing and the record says another. Refs: MGIT-105
func TestCommit_MessageFlagWithFileFlag_Refused_NamesBothFlags(t *testing.T) {
	repo := seedRepoForMessageFile(t)
	headBefore := headCommitID(t, repo)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte("from the file\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))

	tests := []struct {
		name string
		args []string
	}{
		{name: "shorthand_m", args: []string{"commit", "-a", "--task-id", "MGIT-105", "-m", "inline", "-F", msgFile}},
		{name: "long_message", args: []string{"commit", "-a", "--task-id", "MGIT-105", "--message", "inline", "--file", msgFile}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCLI(t, tt.args...)
			require.Error(t, err, "two message sources must be refused, not silently resolved")
			assert.Contains(t, err.Error(), "--message", "the refusal must name the inline flag")
			assert.Contains(t, err.Error(), "--file", "the refusal must name the file flag")
			assert.Equal(t, headBefore, headCommitID(t, repo),
				"a refused commit must leave the branch tip untouched")
		})
	}
}

// TestCommit_MessageFileUnreadable_CommitsNothing: a partial commit carrying
// the wrong message is worse than a failed command, so the read happens before
// anything is staged or written. Refs: MGIT-105
func TestCommit_MessageFileUnreadable_CommitsNothing(t *testing.T) {
	repo := seedRepoForMessageFile(t)
	headBefore := headCommitID(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"), []byte("work\n"), 0o600))

	emptyFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, os.WriteFile(emptyFile, nil, 0o600))

	tests := []struct {
		name string
		path string
	}{
		{name: "missing_file", path: filepath.Join(t.TempDir(), "no-such-message.txt")},
		{name: "directory_instead_of_file", path: t.TempDir()},
		{name: "empty_file", path: emptyFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runCLI(t, "commit", "-a", "--task-id", "MGIT-105", "-F", tt.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--file", "the error must name the flag that failed")

			assert.Equal(t, headBefore, headCommitID(t, repo),
				"nothing may be committed when the message could not be read")
			assert.Empty(t, fileInCommit(t, repo, headCommitID(t, repo), "work.txt"),
				"the work must not have landed under a wrong or generated message")
		})
	}

	// Nor was anything left staged: had -a run before the message was read, the
	// next bare commit would silently record the work under some other message.
	err := runCLI(t, "commit", "--task-id", "MGIT-105", "-m", "whatever a failed -F left staged")
	require.Error(t, err, "a failed --file must leave the staging area untouched")
	assert.Contains(t, err.Error(), "nothing to commit")
}

// TestCommit_MessageFile_DryRun_ShowsTheFileMessageAndCommitsNothing keeps
// --dry-run honest about which message would be recorded. Refs: MGIT-105
func TestCommit_MessageFile_DryRun_ShowsTheFileMessageAndCommitsNothing(t *testing.T) {
	repo := seedRepoForMessageFile(t)
	headBefore := headCommitID(t, repo)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte(nastyMessage), 0o600))

	out, err := runCLIOut(t, "commit", "-a", "--task-id", "MGIT-105", "-F", msgFile, "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "krun_start_enter", "the dry run must report the message it would record")
	assert.Equal(t, headBefore, headCommitID(t, repo), "--dry-run must not commit")
}
