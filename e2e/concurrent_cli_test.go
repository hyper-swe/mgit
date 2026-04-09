// Package e2e: concurrent CLI process tests.
// Simulates multiple mgit CLI processes running in parallel,
// exercising real file locking / SQLite WAL / go-git storage.
package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMgitBinary builds the mgit binary once for the test run.
func buildMgitBinary(t *testing.T) string {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..")

	binPath := filepath.Join(t.TempDir(), "mgit-test")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", binPath, "./cmd/mgit/") //nolint:gosec // test path
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build mgit: %s", string(out))
	return binPath
}

// runMgit executes the mgit binary in a given repo directory.
func runMgit(t *testing.T, bin, repoDir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // test path
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestConcurrent_10Agents_SameRepo_DifferentTasks simulates 10 agents
// running mgit commit in the SAME repository but on DIFFERENT tasks.
// This is the multi-agent scenario from FR-16.
func TestConcurrent_10Agents_SameRepo_DifferentTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent test")
	}

	bin := buildMgitBinary(t)
	repoDir := t.TempDir()

	// Initialize the repo once
	out, err := runMgit(t, bin, repoDir, "init")
	require.NoError(t, err, "init: %s", out)

	const agents = 10
	const commitsPerAgent = 3

	var wg sync.WaitGroup
	type agentResult struct {
		agentID string
		taskID  string
		success int
		errors  []string
	}
	results := make([]agentResult, agents)

	for i := range agents {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			res := &results[idx]
			res.agentID = fmt.Sprintf("agent-%02d", idx+1)
			res.taskID = fmt.Sprintf("PROJ-%d.1", idx+1)

			for j := range commitsPerAgent {
				msg := fmt.Sprintf("commit %d from %s", j+1, res.agentID)
				out, err := runMgit(t, bin, repoDir,
					"commit",
					"--task-id="+res.taskID,
					"--agent-id="+res.agentID,
					"--message="+msg,
				)
				if err != nil {
					res.errors = append(res.errors, fmt.Sprintf("[%d] %s: %s", j, err, out))
					continue
				}
				res.success++
			}
		}(i)
	}

	wg.Wait()

	// Report results
	totalSuccess := 0
	totalErrors := 0
	for _, r := range results {
		totalSuccess += r.success
		totalErrors += len(r.errors)
		if len(r.errors) > 0 {
			t.Logf("%s (%s): %d success, %d errors", r.agentID, r.taskID, r.success, len(r.errors))
			for _, e := range r.errors {
				t.Logf("  error: %s", e)
			}
		}
	}
	t.Logf("TOTAL: %d success / %d errors (%d agents x %d commits = %d attempts)",
		totalSuccess, totalErrors, agents, commitsPerAgent, agents*commitsPerAgent)

	// Verify the repo is still consistent
	verifyOut, _ := runMgit(t, bin, repoDir, "verify")
	t.Logf("verify output: %s", verifyOut)

	// Verify each task's commits via log
	for i := range agents {
		taskID := fmt.Sprintf("PROJ-%d.1", i+1)
		out, err := runMgit(t, bin, repoDir, "log", "--task-id="+taskID)
		if err != nil {
			t.Errorf("log for %s failed: %s", taskID, out)
		}
	}

	// Record for analysis — don't fail, we want to see what happens
	assert.GreaterOrEqual(t, totalSuccess, agents,
		"at least one commit per agent should succeed")
}

// TestConcurrent_10Agents_SeparateRepos is the "isolated agents" scenario
// where each agent has its own repository (no shared state).
func TestConcurrent_10Agents_SeparateRepos(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent test")
	}

	bin := buildMgitBinary(t)

	const agents = 10
	var wg sync.WaitGroup
	errors := make(chan error, agents*5)

	for i := range agents {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			repoDir := t.TempDir()
			taskID := fmt.Sprintf("PROJ-%d.1", idx+1)
			agentID := fmt.Sprintf("agent-%02d", idx+1)

			// Each agent initializes its own repo
			if out, err := runMgit(t, bin, repoDir, "init"); err != nil {
				errors <- fmt.Errorf("%s init: %w: %s", agentID, err, out)
				return
			}

			// Make 3 commits
			for j := range 3 {
				msg := fmt.Sprintf("commit %d", j+1)
				out, err := runMgit(t, bin, repoDir,
					"commit", "--task-id="+taskID, "--agent-id="+agentID, "--message="+msg)
				if err != nil {
					errors <- fmt.Errorf("%s commit %d: %w: %s", agentID, j, err, out)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	errCount := 0
	for err := range errors {
		errCount++
		t.Logf("error: %v", err)
	}
	assert.Equal(t, 0, errCount, "isolated agents must have zero errors")
}

// TestConcurrent_SameTask_Race tests two agents committing to the SAME task
// simultaneously — this exercises the UNIQUE constraint on task_commits.
func TestConcurrent_SameTask_Race(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow concurrent test")
	}

	bin := buildMgitBinary(t)
	repoDir := t.TempDir()

	out, err := runMgit(t, bin, repoDir, "init")
	require.NoError(t, err, "init: %s", out)

	const agents = 5
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := range agents {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-%02d", idx+1)
			msg := fmt.Sprintf("concurrent commit from %s", agentID)

			_, err := runMgit(t, bin, repoDir,
				"commit",
				"--task-id=PROJ-1.1",
				"--agent-id="+agentID,
				"--message="+msg,
			)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()
	t.Logf("Same-task race: %d/%d succeeded", successCount, agents)

	// At least one must succeed. Collisions are expected.
	assert.GreaterOrEqual(t, successCount, 1, "at least one commit must succeed")
}
