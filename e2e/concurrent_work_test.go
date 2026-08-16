// Package e2e: concurrent `mgit work` provisioning — the worker-pool shape.
//
// This is the shape a fleet of agents actually starts with: N agents each run
// `mgit work --task-id <own task>` against ONE repo at the same time. It was
// unasserted until MGIT-120: e2e/concurrent_cli_test.go spawns real processes
// but runs only `commit --allow-empty`, and caps each child at 30s — the same
// order as the lock timeout, so it masks a lock that is held too long.
//
// The tests here assert two things that must hold TOGETHER, because the cheap
// way to fix the first is to break the second:
//   - every one of N concurrent provisions produces a USABLE worktree, and
//   - the FR-16 exclusivity rules still admit exactly one winner when two
//     agents race for the same task or the same branch.
//
// Refs: MGIT-120, FR-16, ADR-009
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workLockTimeoutSeconds is the configured process-lock wait for these tests.
// It is deliberately LOW: the defect is "the lock is held across slow work", so
// a short wait turns a slow critical section into a fast, unambiguous failure
// instead of a multi-minute serialization the test would have to sit through.
// With the lock narrowed to the store mutation, each waiter needs a small
// fraction of this. Refs: MGIT-120
const workLockTimeoutSeconds = 10

// runMgitLong executes the mgit binary in dir with a timeout generous enough
// that a SERIALIZED (unfixed) run reports its own lock error rather than being
// killed by the harness — the distinction the 30s cap in concurrent_cli_test.go
// destroys. Refs: MGIT-120
func runMgitLong(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test path
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seedWorkRepo builds the repo the soak's phase 7 provisions against: a real
// git project with real content, mgit-initialized, already carrying `worktrees`
// materialized task worktrees. The pre-existing worktrees matter — the defect
// appears once the repo is LOADED, which is how it appears in production as a
// worker pool warms up. Refs: MGIT-120
func seedWorkRepo(t *testing.T, bin string, files, worktrees int) string {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "src"), 0o750))
	body := strings.Repeat("some source line of a real project file\n", 40)
	for i := range files {
		p := filepath.Join(repo, "src", fmt.Sprintf("file_%04d.txt", i))
		require.NoError(t, os.WriteFile(p, []byte(fmt.Sprintf("// %d\n%s", i, body)), 0o600))
	}
	gitCmd(t, repo, "init")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-m", "seed")

	out, err := runMgitLong(t, bin, repo, "init")
	require.NoError(t, err, "mgit init: %s", out)
	out, err = runMgitLong(t, bin, repo, "config", "set",
		"locks.timeout_seconds", fmt.Sprint(workLockTimeoutSeconds))
	require.NoError(t, err, "config set locks.timeout_seconds: %s", out)

	for i := 1; i <= worktrees; i++ {
		task := fmt.Sprintf("SEED-%d", i)
		out, err := runMgitLong(t, bin, repo, "work", "wt-"+task, "--task-id", task)
		require.NoError(t, err, "seed worktree %s: %s", task, out)
	}
	return repo
}

// workResult is one concurrent provision's outcome.
type workResult struct {
	task string
	out  string
	err  error
}

// runWorkConcurrently launches one `mgit work` per element of tasks, all at
// once, and returns their results in order. Every invocation gets its own
// distinct path. Refs: MGIT-120
func runWorkConcurrently(t *testing.T, bin, repo string, tasks []string, extraArgs ...string) []workResult {
	t.Helper()
	results := make([]workResult, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, task string) {
			defer wg.Done()
			args := append([]string{"work", "wt-" + task, "--task-id", task,
				"--agent-id", "agent-" + task}, extraArgs...)
			out, err := runMgitLong(t, bin, repo, args...)
			results[idx] = workResult{task: task, out: out, err: err}
		}(i, task)
	}
	wg.Wait()
	return results
}

// assertUsableWorktree proves the provision produced a WORKING worktree, not
// merely a command that exited 0: the linked-worktree marker binds the right
// task and branch, the project's content was materialized, the registry knows
// it, and — the thing an agent does next — a commit from inside it lands on the
// task's branch. Refs: MGIT-120, FR-16
func assertUsableWorktree(t *testing.T, bin, repo, task, branch string) {
	t.Helper()
	wtPath := filepath.Join(repo, "wt-"+task)

	marker, err := os.ReadFile(filepath.Join(wtPath, ".mgit", "worktree")) //nolint:gosec // test path
	require.NoError(t, err, "worktree %s has no linked-worktree marker", task)
	assert.Contains(t, string(marker), `"task": "`+task+`"`, "marker binds the wrong task")
	assert.Contains(t, string(marker), `"branch": "`+branch+`"`, "marker binds the wrong branch")

	seeded, err := os.ReadFile(filepath.Join(wtPath, "src", "file_0000.txt")) //nolint:gosec // test path
	require.NoError(t, err, "worktree %s materialized no project content", task)
	assert.Contains(t, string(seeded), "some source line", "materialized content is truncated")

	out, err := runMgitLong(t, bin, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err, "worktree list: %s", out)
	assert.Contains(t, out, "wt-"+task, "worktree %s is not in the registry", task)

	// The agent's next step: edit, commit, and see it on the task branch.
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "step.txt"),
		[]byte("work by "+task+"\n"), 0o600))
	out, err = runMgitLong(t, bin, wtPath, "commit", "-a", "-m", "first step in "+task)
	require.NoError(t, err, "commit from inside worktree %s: %s", task, out)
	out, err = runMgitLong(t, bin, wtPath, "log", "--oneline")
	require.NoError(t, err, "log from inside worktree %s: %s", task, out)
	assert.Contains(t, out, "first step in "+task, "the commit did not land on %s", branch)
}

// TestConcurrentWork_DistinctTasks_AllProduceUsableWorktrees is the property
// the fleet soak's phase 7 reported as MGIT-120: N agents provisioning at once
// against a loaded repo. Before the fix `mgit work` held the repo-wide
// exclusive lock across the working-tree fingerprint, the worktree
// materialization and (with --sandbox) the daemon round-trip, so the waiters
// timed out. Refs: MGIT-120, FR-16
func TestConcurrentWork_DistinctTasks_AllProduceUsableWorktrees(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent provisioning test")
	}
	bin := buildMgitBinary(t)
	repo := seedWorkRepo(t, bin, 1500, 3)

	tasks := []string{"C-1", "C-2", "C-3", "C-4", "C-5", "C-6"}
	results := runWorkConcurrently(t, bin, repo, tasks)

	var failed []string
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v: %s", r.task, r.err,
				strings.SplitN(strings.TrimSpace(r.out), "\n", 2)[0]))
		}
	}
	require.Empty(t, failed, "%d of %d concurrent provisions failed:\n%s",
		len(failed), len(tasks), strings.Join(failed, "\n"))

	for _, task := range tasks {
		assertUsableWorktree(t, bin, repo, task, "task/"+task)
	}
}

// TestConcurrentWork_SameTaskTwoPaths_ExactlyOneWinner proves narrowing the
// lock did not trade a timeout for a race. Two agents racing to bind the SAME
// task must produce exactly one worktree and one clear refusal — never two
// worktrees bound to one task (FR-16: no task sharing). Refs: MGIT-120, FR-16
func TestConcurrentWork_SameTaskTwoPaths_ExactlyOneWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent provisioning test")
	}
	bin := buildMgitBinary(t)
	repo := seedWorkRepo(t, bin, 200, 0)

	const task = "RACE-TASK"
	var wg sync.WaitGroup
	results := make([]workResult, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := fmt.Sprintf("wt-race-%d", idx)
			out, err := runMgitLong(t, bin, repo, "work", path, "--task-id", task)
			results[idx] = workResult{task: path, out: out, err: err}
		}(i)
	}
	wg.Wait()

	winners, losers := splitByOutcome(results)
	require.Len(t, winners, 1, "exactly one agent may bind task %s; got %d winners:\n%s",
		task, len(winners), renderResults(results))
	require.Len(t, losers, 1)
	assert.Contains(t, losers[0].out, "task already bound",
		"the loser must be refused by name, not by a raw constraint error: %s", losers[0].out)

	out, err := runMgitLong(t, bin, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err, "worktree list: %s", out)
	assert.Equal(t, 1, countRegistered(out, "] "+task),
		"registry must hold exactly one worktree for %s:\n%s", task, out)
	assert.NoDirExists(t, filepath.Join(repo, losers[0].task),
		"a refused provision must not leave a materialized worktree behind")
}

// TestConcurrentWork_SameBranchTwoTasks_ExactlyOneWinner is the second half of
// the exclusivity guarantee: two DIFFERENT tasks racing for the same branch
// must also produce exactly one winner (FR-16: no branch sharing).
// Refs: MGIT-120, FR-16
func TestConcurrentWork_SameBranchTwoTasks_ExactlyOneWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent provisioning test")
	}
	bin := buildMgitBinary(t)
	repo := seedWorkRepo(t, bin, 200, 0)

	// The contested branch must already exist: `mgit work --branch <name>` binds
	// an existing branch, so pre-creating it makes the branch — not the task —
	// the only contested resource.
	const shared = "task/SHARED-BR"
	out, err := runMgitLong(t, bin, repo, "branch", "--task-id", "SHARED-BR")
	require.NoError(t, err, "create contested branch: %s", out)

	var wg sync.WaitGroup
	results := make([]workResult, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task := fmt.Sprintf("RACE-BR-%d", idx)
			out, err := runMgitLong(t, bin, repo, "work", "wt-"+task,
				"--task-id", task, "--branch", shared)
			results[idx] = workResult{task: "wt-" + task, out: out, err: err}
		}(i)
	}
	wg.Wait()

	winners, losers := splitByOutcome(results)
	require.Len(t, winners, 1, "exactly one worktree may check out %s; got %d winners:\n%s",
		shared, len(winners), renderResults(results))
	require.Len(t, losers, 1)
	assert.Contains(t, losers[0].out, "branch checked out in another worktree",
		"the loser must be refused by name, not by a raw constraint error: %s", losers[0].out)

	out, err = runMgitLong(t, bin, repo, "worktree", "list", "--porcelain")
	require.NoError(t, err, "worktree list: %s", out)
	assert.Equal(t, 1, countRegistered(out, "["+shared+"]"),
		"registry must hold exactly one worktree on %s:\n%s", shared, out)
	assert.NoDirExists(t, filepath.Join(repo, losers[0].task),
		"a refused provision must not leave a materialized worktree behind")
}

// countRegistered counts the `worktree list --porcelain` LINES containing
// marker. Counting lines, not substrings, matters: a porcelain line repeats the
// task id in both the branch and the task column. Refs: MGIT-120
func countRegistered(porcelain, marker string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(porcelain), "\n") {
		if strings.Contains(line, marker) {
			n++
		}
	}
	return n
}

// splitByOutcome partitions results into successes and failures.
func splitByOutcome(results []workResult) (winners, losers []workResult) {
	for _, r := range results {
		if r.err == nil {
			winners = append(winners, r)
		} else {
			losers = append(losers, r)
		}
	}
	return winners, losers
}

// renderResults formats every result for a failure message, so a broken run
// shows what each agent actually got.
func renderResults(results []workResult) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "  %s: err=%v out=%q\n", r.task, r.err, strings.TrimSpace(r.out))
	}
	return b.String()
}
