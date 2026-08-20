package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-106: `mgit squash` and `mgit merge` took only --message, so any message
// carrying shell metacharacters had to survive a round trip through the shell.
// The squash message is the more exposed of the two: it is the single message
// that leaves mgit's store for the user's REAL git via --to-git, so a quoting
// accident there escapes into a repository mgit does not own.
//
// These tests therefore assert on BYTES and a hash — never a substring. A
// substring assertion passes on a truncated message, which is the exact failure
// being defended against. Refs: FR-2.9, FR-7, FR-8.4, MGIT-105, MGIT-106

// nastySquashMessage carries every character class that dies in a shell —
// backticks, both quote kinds, a command substitution — plus a literal tab,
// consecutive blank lines, and a trailing blank line.
const nastySquashMessage = "squash(MGIT-106): quote `mgit squash -F` and $(cat msg.txt)\n" +
	"\n" +
	"The body names \"double quotes\", 'single quotes', a backticked\n" +
	"`--to-git`, a literal tab ->\there<- and a substitution $(echo pwned)\n" +
	"that must never reach a shell.\n" +
	"\n" +
	"\n" +
	"Two blank lines precede this line; one follows the last.\n" +
	"\n" +
	"Refs: MGIT-106\n" +
	"\n"

// messageDigest is the second, independent assertion on a message's integrity:
// a digest cannot pass on a prefix of the expected bytes.
func messageDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// branchHeadCommitID returns the commit a branch ref points at, read through
// the store layer (never by shelling out to git).
func branchHeadCommitID(t *testing.T, repoRoot, branch string) string {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	b, err := gitstore.NewBranchStore(r).GetBranch(context.Background(), branch)
	require.NoError(t, err)
	return b.HeadCommit
}

// branchExists reports whether a branch ref is present.
func branchExists(t *testing.T, repoRoot, branch string) bool {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	_, err = gitstore.NewBranchStore(r).GetBranch(context.Background(), branch)
	return err == nil
}

// patchSubjectEnvelope is everything mgit puts on the Subject line ahead of the
// message: [PATCH] is format-patch's own marker and [squashed] is the FR-7
// squash marker. Both are leading bracket groups, which git-mailinfo strips
// when a reviewer runs `git am` — verified live against real git in
// TestSquash_MessageFile_GitAm_RecordsTheCallersSubject, not assumed here.
//
// Requiring the exact literal is itself an assertion: if mgit ever grew a third
// marker, or reordered these, the lookup fails rather than quietly absorbing
// the new bytes into "the envelope". Refs: FR-7, MGIT-106
const patchSubjectEnvelope = "Subject: [PATCH] [squashed] "

// patchMessage recovers the commit message from a git format-patch mbox.
//
// The mbox carries the message as the Subject line, the blank line that ends
// the mail headers, the body, and a structural blank line before the `---`
// separator. Rejoining subject and body across that header blank line — and
// dropping the bracket envelope — is exactly what `git am` does, so this is the
// inverse of the emission. That is the point of the assertion it feeds: the
// bytes a reviewer's git puts back into their repository must be the bytes the
// caller supplied. Refs: MGIT-106
func patchMessage(t *testing.T, patch string) string {
	t.Helper()
	i := strings.Index(patch, patchSubjectEnvelope)
	require.GreaterOrEqual(t, i, 0,
		"the patch must carry the expected Subject envelope %q", patchSubjectEnvelope)
	rest := patch[i+len(patchSubjectEnvelope):]
	j := strings.Index(rest, "\n---\n")
	require.GreaterOrEqual(t, j, 0, "the patch header must be terminated by ---")
	return rest[:j]
}

// seedTaskForSquash initializes a repo, records two micro-commits for taskID,
// and returns the repo root (already the working directory). It never assumes
// the test process started inside an mgit repository. Refs: MGIT-106
func seedTaskForSquash(t *testing.T, taskID string) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "alpha.txt"), []byte("alpha\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "first step"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "beta.txt"), []byte("beta\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "second step"))
	return repo
}

// TestSquash_MessageFile_NastyMessage_SurvivesIntoGitPatchByteIdentical is the
// acceptance test for MGIT-106: the file's bytes are the recorded squash
// message, AND they reach the --to-git export unchanged — the artifact a human
// reviewer reads outside mgit entirely. Refs: FR-7, MGIT-106
func TestSquash_MessageFile_NastyMessage_SurvivesIntoGitPatchByteIdentical(t *testing.T) {
	const taskID = "MGIT-106"
	repo := seedTaskForSquash(t, taskID)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte(nastySquashMessage), 0o600))
	patchFile := filepath.Join(t.TempDir(), "task.mbox")

	require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "-F", msgFile,
		"--to-git", "--to-git-output", patchFile))

	// 1. The RECORDED message is the file's bytes, with nothing appended: the
	//    micro-commit summary mgit generates for itself must not grow onto a
	//    message the caller wrote.
	recorded := commitMessageBytes(t, repo, branchHeadCommitID(t, repo, "task/"+taskID))
	require.Equal(t, nastySquashMessage, recorded,
		"the recorded squash message must be the file's bytes verbatim")
	require.Equal(t, messageDigest(nastySquashMessage), messageDigest(recorded),
		"digest of the recorded message must equal the digest of the file")
	require.Len(t, recorded, len(nastySquashMessage),
		"no byte may be trimmed, added or normalized")

	// 2. The same bytes survive into --to-git, the artifact that leaves mgit.
	patch, err := os.ReadFile(patchFile) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	exported := patchMessage(t, string(patch))
	require.Equal(t, nastySquashMessage, exported,
		"the --to-git patch must carry the message byte-identical")
	require.Equal(t, messageDigest(nastySquashMessage), messageDigest(exported),
		"digest of the exported message must equal the digest of the file")

	// The patch must be a real patch, not an empty shell that happens to carry
	// the right header. Refs: MGIT-112
	assert.Contains(t, string(patch), "diff --git ", "the export must carry diff hunks")
}

// TestMerge_MessageFile_NastyMessage_RecordsBytesVerbatim: same contract on the
// merge commit's message. Refs: FR-8.4, MGIT-106
func TestMerge_MessageFile_NastyMessage_RecordsBytesVerbatim(t *testing.T) {
	repo := seedBranchForMerge(t)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte(nastySquashMessage), 0o600))

	require.NoError(t, runCLI(t, "merge", "feature", "--no-ff", "-F", msgFile))

	recorded := commitMessageBytes(t, repo, headCommitID(t, repo))
	require.Equal(t, nastySquashMessage, recorded,
		"the recorded merge message must be the file's bytes verbatim")
	require.Equal(t, messageDigest(nastySquashMessage), messageDigest(recorded),
		"digest of the recorded message must equal the digest of the file")
	require.Len(t, recorded, len(nastySquashMessage), "no byte may be trimmed or added")
}

// seedBranchForMerge builds a repo with a `feature` branch holding one commit,
// with main checked out — ready for `mgit merge feature --no-ff`.
func seedBranchForMerge(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-106", "-m", "seed"))

	require.NoError(t, runCLI(t, "checkout", "-b", "feature"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-106", "-m", "feature work"))
	require.NoError(t, runCLI(t, "checkout", "main"))
	return repo
}

// TestSquashMerge_MessageFileStdin_RecordsBytesVerbatim covers `-F -`, the path
// a programmatic caller uses to avoid a temp file entirely. Refs: MGIT-106
func TestSquashMerge_MessageFileStdin_RecordsBytesVerbatim(t *testing.T) {
	t.Run("squash", func(t *testing.T) {
		const taskID = "MGIT-106"
		repo := seedTaskForSquash(t, taskID)

		require.NoError(t, runCLIWithStdin(t, nastySquashMessage,
			"squash", "--task-id", taskID, "-F", "-"))

		recorded := commitMessageBytes(t, repo, branchHeadCommitID(t, repo, "task/"+taskID))
		require.Equal(t, nastySquashMessage, recorded,
			"stdin must be read as bytes, with no line-based mangling")
		require.Equal(t, messageDigest(nastySquashMessage), messageDigest(recorded))
	})

	t.Run("merge", func(t *testing.T) {
		repo := seedBranchForMerge(t)

		require.NoError(t, runCLIWithStdin(t, nastySquashMessage,
			"merge", "feature", "--no-ff", "-F", "-"))

		recorded := commitMessageBytes(t, repo, headCommitID(t, repo))
		require.Equal(t, nastySquashMessage, recorded,
			"stdin must be read as bytes, with no line-based mangling")
		require.Equal(t, messageDigest(nastySquashMessage), messageDigest(recorded))
	})
}

// TestSquashMerge_MessageFlagWithFileFlag_Refused_NamesBothFlags: silently
// preferring one source would reintroduce the defect class — the caller
// believes it recorded one thing and the record says another. Refs: MGIT-106
func TestSquashMerge_MessageFlagWithFileFlag_Refused_NamesBothFlags(t *testing.T) {
	const taskID = "MGIT-106"

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		args  func(msgFile string) []string
	}{
		{
			name:  "squash_shorthand",
			setup: func(t *testing.T) string { return seedTaskForSquash(t, taskID) },
			args: func(f string) []string {
				return []string{"squash", "--task-id", taskID, "-m", "inline", "-F", f}
			},
		},
		{
			name:  "squash_long",
			setup: func(t *testing.T) string { return seedTaskForSquash(t, taskID) },
			args: func(f string) []string {
				return []string{"squash", "--task-id", taskID, "--message", "inline", "--file", f}
			},
		},
		{
			name:  "merge_shorthand",
			setup: seedBranchForMerge,
			args: func(f string) []string {
				return []string{"merge", "feature", "--no-ff", "-m", "inline", "-F", f}
			},
		},
		{
			name:  "merge_long",
			setup: seedBranchForMerge,
			args: func(f string) []string {
				return []string{"merge", "feature", "--no-ff", "--message", "inline", "--file", f}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := tt.setup(t)
			headBefore := headCommitID(t, repo)

			msgFile := filepath.Join(t.TempDir(), "msg.txt")
			require.NoError(t, os.WriteFile(msgFile, []byte("from the file\n"), 0o600))

			err := runCLI(t, tt.args(msgFile)...)
			require.Error(t, err, "two message sources must be refused, not silently resolved")
			assert.Contains(t, err.Error(), "--message", "the refusal must name the inline flag")
			assert.Contains(t, err.Error(), "--file", "the refusal must name the file flag")

			assert.Equal(t, headBefore, headCommitID(t, repo),
				"a refused command must leave the branch tip untouched")
			assert.False(t, branchExists(t, repo, "task/"+taskID),
				"a refused squash must not have created the task branch")
		})
	}
}

// TestSquash_MessageFileUnreadable_LeavesRepositoryUnchanged: resolving the
// message is the FIRST act of RunE, so a failure to read it writes nothing —
// no squash commit, no task branch, no patch file. Refs: MGIT-105, MGIT-106
func TestSquash_MessageFileUnreadable_LeavesRepositoryUnchanged(t *testing.T) {
	const taskID = "MGIT-106"

	emptyDir := t.TempDir()
	emptyFile := filepath.Join(emptyDir, "empty.txt")
	require.NoError(t, os.WriteFile(emptyFile, nil, 0o600))

	tests := []struct {
		name string
		path string
	}{
		{name: "missing_file", path: filepath.Join(emptyDir, "no-such-message.txt")},
		{name: "directory_instead_of_file", path: emptyDir},
		{name: "empty_file", path: emptyFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := seedTaskForSquash(t, taskID)
			headBefore := headCommitID(t, repo)
			patchFile := filepath.Join(t.TempDir(), "task.mbox")

			err := runCLI(t, "squash", "--task-id", taskID, "-F", tt.path,
				"--to-git", "--to-git-output", patchFile)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--file", "the error must name the flag that failed")

			assert.Equal(t, headBefore, headCommitID(t, repo),
				"nothing may be recorded when the message could not be read")
			assert.False(t, branchExists(t, repo, "task/"+taskID),
				"no task branch may exist after a squash that never ran")
			assert.NoFileExists(t, patchFile, "no patch may be exported")

			// The task is still squashable: the failure consumed no state.
			require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "-m", "recovered"))
			assert.Equal(t, "recovered",
				commitMessageBytes(t, repo, branchHeadCommitID(t, repo, "task/"+taskID)))
		})
	}
}

// TestMerge_MessageFileUnreadable_LeavesRepositoryUnchanged mirrors the squash
// case: no merge commit, and the branch tip is where it was. Refs: MGIT-106
func TestMerge_MessageFileUnreadable_LeavesRepositoryUnchanged(t *testing.T) {
	repo := seedBranchForMerge(t)
	headBefore := headCommitID(t, repo)

	err := runCLI(t, "merge", "feature", "--no-ff", "-F",
		filepath.Join(t.TempDir(), "no-such-message.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--file", "the error must name the flag that failed")

	assert.Equal(t, headBefore, headCommitID(t, repo),
		"a merge whose message could not be read must not move the branch")
	assert.Empty(t, fileInCommit(t, repo, headCommitID(t, repo), "feature.txt"),
		"the source branch's work must not have landed under a generated message")
}

// TestSquash_InlineMessage_RecordedVerbatim pins the other half of the
// verbatim contract: -m is the same message source as -F, so mgit's generated
// micro-commit summary must not be appended to it either. Refs: MGIT-106
func TestSquash_InlineMessage_RecordedVerbatim(t *testing.T) {
	const taskID = "MGIT-106"
	repo := seedTaskForSquash(t, taskID)

	const msg = "release: consolidate MGIT-106"
	require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "-m", msg))

	recorded := commitMessageBytes(t, repo, branchHeadCommitID(t, repo, "task/"+taskID))
	require.Equal(t, msg, recorded,
		"a caller-supplied squash message is recorded exactly as given")
}

// TestSquash_NoMessage_StillSummarizesMicroCommits: removing the appended
// summary from a SUPPLIED message must not remove it from the one mgit
// generates, where there is no caller intent to contradict. Refs: MGIT-106
func TestSquash_NoMessage_StillSummarizesMicroCommits(t *testing.T) {
	const taskID = "MGIT-106"
	repo := seedTaskForSquash(t, taskID)

	require.NoError(t, runCLI(t, "squash", "--task-id", taskID))

	recorded := commitMessageBytes(t, repo, branchHeadCommitID(t, repo, "task/"+taskID))
	assert.Contains(t, recorded, "Squashed from 2 micro-commits")
	assert.Contains(t, recorded, "first step")
	assert.Contains(t, recorded, "second step")
}

// gitCapture runs a real git command in dir and returns its stdout.
func gitCapture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=dev@example.com", "-c", "user.name=dev"}, args...)
	cmd := exec.Command("git", full...) //nolint:gosec // fixed args, test only
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return string(out)
}

// collapseBlankRuns rewrites runs of blank lines as a single blank line and
// drops trailing blank lines — git's own commit-message cleanup, applied to
// both sides of the `git am` comparison so the assertion pins what MGIT
// controls (every other byte) rather than which git version is installed.
func collapseBlankRuns(s string) string {
	var out []string
	prevBlank := false
	for _, line := range strings.Split(s, "\n") {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		prevBlank = blank
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// TestSquash_MessageFile_GitAm_RecordsTheCallersSubject closes the loop the
// unit assertions can only reason about: it applies the exported patch with
// REAL git and checks what a reviewer's repository ends up holding.
//
// It pins two things. First, the recorded SUBJECT is the caller's first line
// exactly — no `[PATCH]`, no `[squashed]`; git-mailinfo strips both bracket
// groups, which is why mgit may carry a marker on the Subject line without it
// reaching anyone's history. Second, the whole message survives modulo git's
// own message cleanup, which is applied to both sides here: any difference
// beyond that is mgit mangling the caller's bytes. Refs: FR-7, MGIT-106
func TestSquash_MessageFile_GitAm_RecordsTheCallersSubject(t *testing.T) {
	const taskID = "MGIT-106"
	seedTaskForSquash(t, taskID)

	msgFile := filepath.Join(t.TempDir(), "msg.txt")
	require.NoError(t, os.WriteFile(msgFile, []byte(nastySquashMessage), 0o600))
	patchFile := filepath.Join(t.TempDir(), "task.mbox")
	require.NoError(t, runCLI(t, "squash", "--task-id", taskID, "-F", msgFile,
		"--to-git", "--to-git-output", patchFile))

	// A REAL git repository, the user's side of the boundary.
	userRepo := t.TempDir()
	gitInit(t, userRepo, nil)
	gitRun(t, userRepo, "am", patchFile)

	wantSubject, _, _ := strings.Cut(nastySquashMessage, "\n")
	assert.Equal(t, wantSubject, strings.TrimSuffix(gitCapture(t, userRepo, "log", "-1", "--format=%s"), "\n"),
		"the reviewer's git must record the caller's subject line, envelope stripped")

	landed := gitCapture(t, userRepo, "log", "-1", "--format=%B")
	require.Equal(t, collapseBlankRuns(nastySquashMessage), collapseBlankRuns(landed),
		"every byte the caller wrote must reach the user's real git")
	require.Equal(t,
		messageDigest(collapseBlankRuns(nastySquashMessage)),
		messageDigest(collapseBlankRuns(landed)),
		"digest of the landed message must equal the digest of the file")
}

// TestSquash_CallerMessage_ProvenanceRidesBelowTheSeparator pins BOTH halves of
// the trade this placement dissolves, in one test, because either half alone is
// a defect that looks like a success.
//
//	half 1  the caller's bytes reach the user's real git UNCHANGED
//	half 2  the reviewer reading that patch still sees what was collapsed
//
// Half 1 alone is MGIT-106 as first written: byte-identical, and a single opaque
// patch with no record that two micro-commits went into it — the opposite of a
// receipt for the exact audience this product exists for. Half 2 alone is the
// defect MGIT-106 closed: a summary appended to a message someone else wrote,
// making the record say something the caller did not.
//
// Git's `---` separator is what makes both true at once: everything between it
// and the first `diff --git` is discarded by `git am` and read by a human. So
// this asserts the provenance IS in the patch bytes and IS NOT in the message
// bytes — the same fact from both sides. Refs: MGIT-106, FR-7
func TestSquash_CallerMessage_ProvenanceRidesBelowTheSeparator(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "PROV-1", "-m", "step one"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "PROV-1", "-m", "step two"))

	msg := "my own message\n\nwith `backticks` and $(echo x)\n"
	msgPath := filepath.Join(dir, "msg.txt")
	require.NoError(t, os.WriteFile(msgPath, []byte(msg), 0o600))

	out := filepath.Join(dir, "out.patch")
	require.NoError(t, runCLI(t, "squash", "--task-id", "PROV-1", "-F", msgPath,
		"--to-git", "--to-git-output", out))
	patch, err := os.ReadFile(out) //nolint:gosec // test-controlled path, same as the sibling assertions in this file
	require.NoError(t, err)
	text := string(patch)

	// The separator is the seam the whole design rests on.
	sep := strings.Index(text, "\n---\n")
	require.Positive(t, sep, "the patch has no --- separator, so nothing can ride below it")
	above, below := text[:sep], text[sep:]

	// HALF 2: provenance is present, and present BELOW the separator.
	assert.Contains(t, below, "Squashed from 2 micro-commits:",
		"a reviewer reading this patch in their own git cannot tell it collapsed anything")
	assert.Contains(t, below, "step one")
	assert.Contains(t, below, "step two")

	// HALF 1: and none of it is in the message region, so `git am` records only
	// what the caller wrote. Asserted as a negative on the region above the
	// separator, which is the part git keeps.
	assert.NotContains(t, above, "Squashed from",
		"provenance leaked into the message region: git am would record bytes the caller never wrote")
	assert.NotContains(t, above, "step one")
	assert.Contains(t, above, "with `backticks` and $(echo x)",
		"the caller's own body must still be in the message region")
}
