package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-123: `mgit status` ran the ADR-008 auto-resync, which staged the WHOLE
// working tree into the .mgit base. A user's uncommitted edit was absorbed into
// an untagged [mgit-sync] commit, so status reported clean (it had just made it
// so), `add` staged truthfully against a base that already matched, and `commit`
// refused with "nothing to commit ... identical to its parent". The work was not
// lost from disk but became UNCOMMITTABLE to its task and invisible to status —
// landing in housekeeping rather than in a task-tagged micro-commit a reviewer
// can trace.
//
// These tests assert on COMMITTED CONTENT, never on exit codes: on the broken
// path `mgit add` exits 0 and only the later `commit` fails, so an exit-code
// assertion is exactly what let this reach a user. Refs: MGIT-123, ADR-008, FR-1.1

// projectWithGit makes dir a real git repo with one commit and chdirs into it,
// so the production gitref reader resolves a true local HEAD (the sync gate
// degrades to a no-op without one, which would make these tests vacuous).
func projectWithGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o600))
	_, err = wt.Add("seed.txt")
	require.NoError(t, err)
	sig := &object.Signature{Name: "dev", Email: "dev@x", When: time.Unix(0, 0).UTC()}
	_, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	return dir
}

// openStore opens the .mgit store for direct assertions on what was recorded.
func openStore(t *testing.T, dir string) (*gitstore.Repository, *gitstore.CommitStore) {
	t.Helper()
	repo, err := gitstore.Open(dir, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	return repo, gitstore.NewCommitStore(repo)
}

// headCommit returns the base-branch tip as a model commit.
func headCommit(t *testing.T, dir string) (*gitstore.Repository, string) {
	t.Helper()
	repo, _ := openStore(t, dir)
	head, err := repo.Head()
	require.NoError(t, err)
	return repo, head
}

// TestStatus_ThenAddCommit_RecordsContentUnderTask is the field reproduction:
// the ONLY difference from the working path is a prior `mgit status`.
// Refs: MGIT-123, ADR-008 §3
func TestStatus_ThenAddCommit_RecordsContentUnderTask(t *testing.T) {
	dir := projectWithGit(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "only.txt"), []byte("content-B\n"), 0o600))

	require.NoError(t, runCLI(t, "status"))
	require.NoError(t, runCLI(t, "add", "only.txt"))
	require.NoError(t, runCLI(t, "commit", "--task-id", "MGIT-123", "-m", "add only.txt"))

	repo, head := headCommit(t, dir)
	cs := gitstore.NewCommitStore(repo)
	got, err := cs.GetFileFromCommit(context.Background(), head, "only.txt")
	require.NoError(t, err, "the edit must be recorded in a commit, not swallowed by the base")
	assert.Equal(t, "content-B\n", string(got))

	c, err := cs.GetCommit(context.Background(), head)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-123", c.TaskID.String(), "the content must be attributed to the task, not to [mgit-sync]")
}

// TestStatus_ThenCommitAll_RecordsContentUnderTask covers the one-step path:
// `commit -a` was NOT immune to the absorption. Refs: MGIT-123
func TestStatus_ThenCommitAll_RecordsContentUnderTask(t *testing.T) {
	dir := projectWithGit(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "only.txt"), []byte("content-C\n"), 0o600))

	require.NoError(t, runCLI(t, "status"))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-123", "-m", "add only.txt"))

	repo, head := headCommit(t, dir)
	cs := gitstore.NewCommitStore(repo)
	got, err := cs.GetFileFromCommit(context.Background(), head, "only.txt")
	require.NoError(t, err)
	assert.Equal(t, "content-C\n", string(got))

	c, err := cs.GetCommit(context.Background(), head)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-123", c.TaskID.String())
}

// TestStatus_RepeatedOnUncommittedEdit_BaseGainsNoCommit asserts the base does
// not grow for an uncommitted edit no matter how often a read verb runs — the
// [mgit-sync] absorption itself, independent of any later commit.
// Refs: MGIT-123, ADR-008 §3,§6
func TestStatus_RepeatedOnUncommittedEdit_BaseGainsNoCommit(t *testing.T) {
	dir := projectWithGit(t)
	// Settle any first-run base import so the assertion isolates the EDIT.
	require.NoError(t, runCLI(t, "status"))
	_, before := headCommit(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("uncommitted\n"), 0o600))
	for i := 0; i < 3; i++ {
		require.NoError(t, runCLI(t, "status"))
		require.NoError(t, runCLI(t, "diff", "--staged"))
	}

	_, after := headCommit(t, dir)
	assert.Equal(t, before, after, "read verbs must not advance the base for an uncommitted edit")

	// And the edit is still committable to a task, with its content.
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-123", "-m", "edit"))
	repo, head := headCommit(t, dir)
	got, err := gitstore.NewCommitStore(repo).GetFileFromCommit(context.Background(), head, "edit.txt")
	require.NoError(t, err)
	assert.Equal(t, "uncommitted\n", string(got))
}

// TestStatus_ReportsUncommittedWork verifies the user-visible half of the fix:
// status must SHOW the uncommitted file rather than report a clean tree it just
// created. Refs: MGIT-123
func TestStatus_ReportsUncommittedWork(t *testing.T) {
	dir := projectWithGit(t)
	require.NoError(t, runCLI(t, "status")) // settle the first-run base import
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("x\n"), 0o600))

	out, err := runCLIOut(t, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "visible.txt", "status must report uncommitted work, not absorb it")
}

// TestStatus_GitHeadMoved_BaseStillResyncs is the legitimate ADR-008 §3 case the
// fix must NOT disable: when the user's real git HEAD genuinely moves, the base
// resyncs so mgit never diffs against a stale base. Refs: MGIT-123, ADR-008 §3
func TestStatus_GitHeadMoved_BaseStillResyncs(t *testing.T) {
	dir := projectWithGit(t)
	require.NoError(t, runCLI(t, "status"))
	_, before := headCommit(t, dir)

	// The user delivers work through their real git (the MGIT-26 drift ADR-008
	// exists for): git HEAD moves, and the .mgit base is now stale.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "landed.go"), []byte("package landed\n"), 0o600))
	gitCommitAll(t, dir, "landed via git")

	require.NoError(t, runCLI(t, "status"))

	repo, after := headCommit(t, dir)
	require.NotEqual(t, before, after, "a moved git HEAD must still resync the base")
	got, err := gitstore.NewCommitStore(repo).GetFileFromCommit(context.Background(), after, "landed.go")
	require.NoError(t, err, "git-committed content must be absorbed into the base")
	assert.Equal(t, "package landed\n", string(got))
}

// TestStatus_GitHeadMoved_UncommittedWorkStillCommittable is the composed case:
// a resync triggered by a real git commit must still leave the user's SEPARATE
// uncommitted edit out of the base and attributable to a task. This is the
// narrower variant of MGIT-123 that a "resync only when git HEAD moved" fix
// alone would have left open. Refs: MGIT-123
func TestStatus_GitHeadMoved_UncommittedWorkStillCommittable(t *testing.T) {
	dir := projectWithGit(t)
	require.NoError(t, runCLI(t, "status"))

	// An in-flight edit the user intends for their task...
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("in-flight\n"), 0o600))
	// ...while an UNRELATED change is delivered through real git.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "landed.go"), []byte("package landed\n"), 0o600))
	gitCommitOne(t, dir, "landed.go", "landed via git")

	require.NoError(t, runCLI(t, "status"))

	repo, base := headCommit(t, dir)
	cs := gitstore.NewCommitStore(repo)
	_, err := cs.GetFileFromCommit(context.Background(), base, "wip.txt")
	require.Error(t, err, "uncommitted work must not be absorbed even by a legitimate resync")

	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-123", "-m", "wip"))
	_, head := headCommit(t, dir)
	got, err := cs.GetFileFromCommit(context.Background(), head, "wip.txt")
	require.NoError(t, err)
	assert.Equal(t, "in-flight\n", string(got))
}

// TestStatus_GenuineCaseCollision_StillCommits guards the behavior the defect
// was originally MIS-reported as. Two paths differing only in case are put in
// the git tree with plumbing (a case-insensitive host cannot create both in the
// working tree), then checked out; the surviving file is edited and must still
// commit with its content after a `status`. This worked before the fix and must
// keep working. Refs: MGIT-123
func TestStatus_GenuineCaseCollision_StillCommits(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "dev@x")
	runGit(t, dir, "config", "user.name", "dev")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.tmp"), []byte("export const a = 1;\n"), 0o600))

	blob := runGit(t, dir, "hash-object", "-w", "blob.tmp")
	// Two tree entries differing only in case — the genuine collision.
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",src/auth.tsx")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",src/Auth.tsx")
	runGit(t, dir, "commit", "-m", "collide")
	require.NoError(t, os.Remove(filepath.Join(dir, "blob.tmp")))
	runGit(t, dir, "checkout", "--", ".")

	t.Chdir(dir)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, runCLI(t, "status"))

	// How many files land depends on the FILESYSTEM, not on mgit: a
	// case-insensitive host (macOS, the reported platform) collides the two
	// tree entries onto one file, while a case-sensitive one (Linux, and CI)
	// materializes both. Asserting one of those shapes fails on the other
	// platform for a reason that has nothing to do with the property under
	// test — which is that a genuine collision still commits its CONTENT.
	// So read the directory for the REAL casing and assert the shape that
	// this filesystem actually produces. os.Stat would match either name on a
	// case-insensitive host and tell us nothing about what mgit sees when it
	// walks the tree. Refs: MGIT-123
	entries, err := os.ReadDir(filepath.Join(dir, "src"))
	require.NoError(t, err)
	if len(entries) == 1 {
		t.Logf("case-insensitive filesystem: the two entries collided onto %q", entries[0].Name())
	} else {
		require.Len(t, entries, 2, "a case-sensitive filesystem must materialize both entries")
		t.Logf("case-sensitive filesystem: both entries materialized")
	}
	rel := "src/" + entries[0].Name()
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte("export const a = 2;\n"), 0o600))

	require.NoError(t, runCLI(t, "status"))
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-123", "-m", "edit collider"))

	repo, head := headCommit(t, dir)
	got, err := gitstore.NewCommitStore(repo).GetFileFromCommit(context.Background(), head, rel)
	require.NoError(t, err, "a genuine case collision must still commit its content")
	assert.Equal(t, "export const a = 2;\n", string(got))
}

// runGit runs one git plumbing/porcelain command in dir and returns its trimmed
// stdout. Test-only: production code never shells out to git (ADR-001).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	//nolint:gosec // test-only helper; args are literals from this file, not user input
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(trimTrailingNewline(out))
}

// gitCommitAll stages and commits everything in the project's real git.
func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.email=dev@x", "-c", "user.name=dev", "commit", "-m", msg)
}

// gitCommitOne stages and commits a single path in the project's real git,
// leaving any other working-tree change uncommitted.
func gitCommitOne(t *testing.T, dir, path, msg string) {
	t.Helper()
	runGit(t, dir, "add", path)
	runGit(t, dir, "-c", "user.email=dev@x", "-c", "user.name=dev", "commit", "-m", msg)
}

// trimTrailingNewline drops trailing newlines from command output.
func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
