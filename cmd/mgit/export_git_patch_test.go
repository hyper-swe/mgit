package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MGIT-112: `mgit export --format git` emitted a well-formed mbox with ZERO
// hunks — the rendering read model.FileDiffs that only the NON-dry-run squash
// ever populated. The output was syntactically valid, so a caller piping it to
// `git apply --allow-empty` (or `git am --allow-empty`) got a clean success and
// an unchanged tree: silent loss on the verb whose only job is getting work OUT
// of mgit. The tests here assert on HUNKS and on APPLIED FILE CONTENT, never on
// the header or on git's exit code — the previous test asserted only the header,
// which a header-only patch satisfies, and that is how the defect survived.
// Refs: MGIT-112, MGIT-33, MGIT-77, FR-7

// runCLICap executes one mgit command and returns its real stdout and stderr
// separately. The commands print through fmt.Fprint to os.Stdout/os.Stderr, so
// a cobra SetOut buffer comes back empty and every output assertion would pass
// vacuously. Separating the streams matters here: stdout must stay a clean,
// pipeable patch while operator notes go to stderr. Refs: MGIT-112
func runCLICap(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	outR, outW, perr := os.Pipe()
	require.NoError(t, perr)
	errR, errW, perr := os.Pipe()
	require.NoError(t, perr)

	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	outC, errC := make(chan string, 1), make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, outR)
		outC <- b.String()
	}()
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, errR)
		errC <- b.String()
	}()

	var cobraOut bytes.Buffer
	root := rootCmd()
	root.SetOut(&cobraOut)
	root.SetErr(&cobraOut)
	root.SetArgs(args)
	runErr := root.Execute()

	os.Stdout, os.Stderr = savedOut, savedErr
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	stdout, stderr = <-outC, <-errC
	stderr += cobraOut.String()
	require.NoError(t, outR.Close())
	require.NoError(t, errR.Close())
	return stdout, stderr, runErr
}

// patchHunks returns the diff portion of a git format-patch: everything from
// the first "diff --git" header up to the mbox trailer, or "" when the patch
// carries no hunks at all. Every equivalence assertion compares THIS, not the
// header — MGIT-112 shipped a patch whose header was perfect. Refs: MGIT-112
func patchHunks(patch string) string {
	i := strings.Index(patch, "diff --git ")
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(patch[i:], "-- \nmgit\n")
}

// exportTaskFixture is one export scenario: files committed under baseTask
// first (so they form the task's fork base), then the changes the task itself
// makes. A "" value in task means the task deletes that path.
type exportTaskFixture struct {
	name         string
	base         map[string]string
	task         map[string]string
	wantInHunks  []string
	wantNotHunks []string
}

// seedExportRepo initializes an mgit repo in a fresh temp dir and applies the
// fixture: the base files under a separate task, then the task's own changes.
// Returns the repo root. Refs: MGIT-112
func seedExportRepo(t *testing.T, f exportTaskFixture, taskID string) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))

	if len(f.base) > 0 {
		writeFixtureFiles(t, dir, f.base)
		require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "BASE-0", "-m", "base state"))
	}
	writeFixtureFiles(t, dir, f.task)
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "task change"))
	return dir
}

// writeFixtureFiles writes each path (creating parent dirs); a "" content
// deletes the path, which is how the fixtures express a task deletion.
func writeFixtureFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if content == "" {
			require.NoError(t, os.Remove(full))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}

// exportFixtures covers the four change shapes the ticket names: added,
// modified, deleted and multi-file. Refs: MGIT-112
func exportFixtures() []exportTaskFixture {
	return []exportTaskFixture{
		{
			name:        "added_file",
			task:        map[string]string{"f.txt": "line one\nline two\n"},
			wantInHunks: []string{"diff --git a/f.txt b/f.txt", "@@ -0,0 +1,2 @@", "+line one", "+line two"},
		},
		{
			name:        "modified_file",
			base:        map[string]string{"b.txt": "alpha\nbeta\n"},
			task:        map[string]string{"b.txt": "alpha\nBETA\ngamma\n"},
			wantInHunks: []string{"diff --git a/b.txt b/b.txt", "-beta", "+BETA", "+gamma"},
		},
		{
			name:         "deleted_file",
			base:         map[string]string{"gone.txt": "remove me\n", "keep.txt": "keep\n"},
			task:         map[string]string{"gone.txt": ""},
			wantInHunks:  []string{"diff --git a/gone.txt b/gone.txt", "-remove me"},
			wantNotHunks: []string{"keep.txt"},
		},
		{
			name: "multi_file",
			base: map[string]string{"old.txt": "one\n"},
			task: map[string]string{
				"old.txt":     "one\ntwo\n",
				"new.txt":     "brand new\n",
				"pkg/sub.txt": "nested\n",
			},
			wantInHunks: []string{
				"diff --git a/old.txt b/old.txt", "+two",
				"diff --git a/new.txt b/new.txt", "+brand new",
				"diff --git a/pkg/sub.txt b/pkg/sub.txt", "+nested",
			},
		},
	}
}

// TestExportFormatGit_MatchesSquashToGit_OnHunks proves the two verbs render
// the SAME diff for the same task. Export runs first (it must not mutate
// state), then squash; the assertion compares hunks, so a header-only patch
// fails it. Refs: MGIT-112, FR-7
func TestExportFormatGit_MatchesSquashToGit_OnHunks(t *testing.T) {
	for _, tt := range exportFixtures() {
		t.Run(tt.name, func(t *testing.T) {
			const taskID = "EXP-1"
			seedExportRepo(t, tt, taskID)

			exportOut, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
			require.NoError(t, err, "export --format git must succeed")
			exportHunks := patchHunks(exportOut)
			require.NotEmpty(t, exportHunks,
				"export --format git produced NO hunks for a non-empty task (MGIT-112)")

			squashOut, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git")
			require.NoError(t, err, "squash --to-git must succeed")
			squashHunks := patchHunks(squashOut)
			require.NotEmpty(t, squashHunks, "squash --to-git produced no hunks")

			assert.Equal(t, squashHunks, exportHunks,
				"export --format git and squash --to-git must render the same diff")
			for _, want := range tt.wantInHunks {
				assert.Contains(t, exportHunks, want)
			}
			for _, unwanted := range tt.wantNotHunks {
				assert.NotContains(t, exportHunks, unwanted,
					"a file the task never touched must not appear in its patch")
			}
		})
	}
}

// TestExportFormatGit_GitApply_ReproducesTaskFileContent is the assertion that
// actually matters: pipe the export into `git apply` in a REAL git repo and
// check the resulting FILE CONTENT. Asserting git's exit code would not catch
// MGIT-112 — `git apply --allow-empty` succeeds on the broken header-only
// output and leaves the tree untouched. Refs: MGIT-112, FR-7
func TestExportFormatGit_GitApply_ReproducesTaskFileContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	for _, tt := range exportFixtures() {
		t.Run(tt.name, func(t *testing.T) {
			const taskID = "EXP-1"
			seedExportRepo(t, tt, taskID)

			patch, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
			require.NoError(t, err)

			patchFile := filepath.Join(t.TempDir(), "task.patch")
			require.NoError(t, os.WriteFile(patchFile, []byte(patch), 0o600))

			// A real git repo holding exactly the task's fork base.
			gitDir := t.TempDir()
			gitInit(t, gitDir, tt.base)
			gitRun(t, gitDir, "apply", patchFile)

			// Assert CONTENT, never the exit code.
			for path, want := range expectedAfterApply(tt) {
				full := filepath.Join(gitDir, path)
				if want == "" {
					_, statErr := os.Stat(full)
					assert.True(t, os.IsNotExist(statErr),
						"%s must be deleted by the applied patch", path)
					continue
				}
				got, readErr := os.ReadFile(full) //nolint:gosec // test path
				require.NoError(t, readErr, "%s must exist after applying the export", path)
				assert.Equal(t, want, string(got),
					"applied export must reproduce %s byte for byte", path)
			}
		})
	}
}

// expectedAfterApply returns the file state a correctly applied export must
// leave behind: the base overlaid with the task's changes.
func expectedAfterApply(f exportTaskFixture) map[string]string {
	want := make(map[string]string, len(f.base)+len(f.task))
	for p, c := range f.base {
		want[p] = c
	}
	for p, c := range f.task {
		want[p] = c // "" means deleted, which the caller checks for absence
	}
	return want
}

// gitInit creates a real git repo seeded with the given files and one commit.
func gitInit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	gitRun(t, dir, "init")
	if len(files) == 0 {
		gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
		return
	}
	writeFixtureFiles(t, dir, files)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "base")
}

// gitRun runs a real git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=dev@example.com", "-c", "user.name=dev"}, args...)
	cmd := exec.Command("git", full...) //nolint:gosec // fixed args, test only
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}

// TestExportFormatGit_EmptyNetChange_ReportedDistinctlyFromFailure covers the
// commit-and-revert case that prompted the upstream report: the net change
// really is nothing. That must be SAID, not silently rendered as an empty
// patch, and it must be distinguishable from a failure — success exit, an
// explicit note on stderr, and no patch bytes on stdout. Refs: MGIT-112
func TestExportFormatGit_EmptyNetChange_ReportedDistinctlyFromFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))

	const taskID = "EXP-EMPTY"
	f := filepath.Join(dir, "tmp.txt")
	require.NoError(t, os.WriteFile(f, []byte("scratch\n"), 0o600))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "add scratch file"))
	require.NoError(t, os.Remove(f))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", taskID, "-m", "revert: drop it again"))

	stdout, stderr, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err, "an empty net change is a legitimate outcome, not a failure")
	assert.Empty(t, patchHunks(stdout), "there genuinely are no hunks to render")
	assert.Empty(t, strings.TrimSpace(stdout),
		"an empty net change must print no patch at all, not a silently-empty one")
	assert.Contains(t, strings.ToLower(stderr), "empty",
		"the empty net change must be stated explicitly so a reviewer mid-recovery "+
			"never has to guess whether the tool failed")
	assert.Contains(t, stderr, taskID)
}

// TestExportFormatGit_UnknownTask_FailsLoudly proves the other side of the
// distinction: a task that cannot be diffed exits non-zero instead of emitting
// an applyable empty patch. Refs: MGIT-112
func TestExportFormatGit_UnknownTask_FailsLoudly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))

	stdout, _, err := runCLICap(t, "export", "--task-id", "NO-SUCH-TASK", "--format", "git")
	require.Error(t, err, "an uncomputable diff must fail loudly, never emit an empty patch")
	assert.Empty(t, strings.TrimSpace(stdout))
}

// TestExportFormatGit_LeavesNoSquashCommitAndNoAuditRecord keeps the dry-run
// intent that was correct all along: export is a READ. It must not index a
// squash commit, must not write an audit record, and must not create the
// task branch. Refs: MGIT-112, FR-12
func TestExportFormatGit_LeavesNoSquashCommitAndNoAuditRecord(t *testing.T) {
	const taskID = "EXP-1"
	seedExportRepo(t, exportFixtures()[0], taskID)

	before, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "json")
	require.NoError(t, err)

	patch, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err)
	require.NotEmpty(t, patchHunks(patch), "precondition: the export carries real hunks")

	after, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, before, after,
		"export must not append an indexed squash commit to the task")

	auditLog, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "audit-log")
	require.NoError(t, err)
	assert.NotContains(t, auditLog, "SQUASH",
		"export must write no audit record — it is a read verb")

	branches, _, err := runCLICap(t, "branch")
	require.NoError(t, err)
	assert.NotContains(t, branches, "task/"+taskID,
		"export must not create the task branch a real squash would")
}

// TestSquashToGit_DryRun_PreviewsTheRealPatch closes the second instance of the
// same defect class: `squash --to-git --dry-run` had no squash commit to diff
// and simply failed ("to commit is empty"). It now renders through the same
// read-only preview as export — a real preview that mutates nothing.
// Refs: MGIT-112, FR-7
func TestSquashToGit_DryRun_PreviewsTheRealPatch(t *testing.T) {
	const taskID = "EXP-1"
	seedExportRepo(t, exportFixtures()[1], taskID)

	dryOut, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git", "--dry-run")
	require.NoError(t, err, "--to-git --dry-run must preview, not fail")
	dryHunks := patchHunks(dryOut)
	require.NotEmpty(t, dryHunks, "the dry-run preview must carry real hunks")

	// It changed nothing, so the real squash that follows renders the same diff.
	realOut, _, err := runCLICap(t, "squash", "--task-id", taskID, "--to-git")
	require.NoError(t, err)
	assert.Equal(t, patchHunks(realOut), dryHunks,
		"the dry-run preview must describe exactly what the real squash produces")
}

// TestExportFormatGit_RepeatedExports_AreIdentical guards the read contract
// from the other direction: exporting twice yields the same hunks, because the
// first export changed nothing that the second could observe. Refs: MGIT-112
func TestExportFormatGit_RepeatedExports_AreIdentical(t *testing.T) {
	const taskID = "EXP-1"
	seedExportRepo(t, exportFixtures()[3], taskID)

	first, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err)
	second, _, err := runCLICap(t, "export", "--task-id", taskID, "--format", "git")
	require.NoError(t, err)

	require.NotEmpty(t, patchHunks(first))
	assert.Equal(t, patchHunks(first), patchHunks(second))
}
