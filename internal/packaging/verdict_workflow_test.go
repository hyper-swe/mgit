package packaging

import (
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE VERDICT GATE IS WIRED INTO CI (MGIT-201): it runs on every pull
// request event AND on every comment event, so a PASS posted after the
// push re-evaluates the head; it runs the gate script; and it writes
// one commit status on the head sha so branch protection can require it.
func TestVerdictGate_IsWiredIntoCI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "verdict.yml"))
	if err != nil {
		t.Fatalf("the verdict gate workflow must exist: %v", err)
	}
	wf := string(raw)
	for _, want := range []string{
		"pull_request:", "issue_comment:", "synchronize", "created", "edited", "deleted",
		"./scripts/verdictgate", "statuses: write", "reviewer-verdict-at-head",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("verdict.yml lacks %q", want)
		}
	}
	// the status description must be the gate's own last line, never
	// `go run`'s "exit status 1" trailer (the reviewer's find on the swe repository's #226):
	// the binary is built and run, and the description is the last line
	// that starts with "verdictgate:"
	for _, want := range []string{"go build -o", "grep '^verdictgate:'"} {
		if !strings.Contains(wf, want) {
			t.Errorf("the status description must come from the gate's own words (%q)", want)
		}
	}
	if strings.Contains(wf, "go run ./scripts/verdictgate -repo") {
		t.Error("`go run` appends 'exit status N' to stderr on failure; the workflow must run a built binary")
	}
	if !strings.Contains(wf, "github.event.issue.pull_request") {
		t.Error("comment events on plain issues must be skipped: the job's condition must read github.event.issue.pull_request")
	}
}

// The gate's verdict lives in the commit STATUS it writes, and only there.
// The job itself must exit clean: a non-zero exit on a fresh head — every
// head, since no verdict exists yet by construction — makes Actions record
// a failing CheckRun under the same name, and check runs are not
// last-write-wins the way statuses are, so that red would outlive every
// later PASS (found on swe's original, 2026-09-09). Refs: MGIT-201
func TestVerdictGate_JobExitsCleanAfterWritingTheStatus(t *testing.T) {
	cfg := readRepoFile(t, ".github/workflows/verdict.yml")
	// The statement, at the step's indentation — the status description may
	// still mention the code in prose.
	assert.NotContains(t, cfg, "\n          exit $code", "the gate's exit code is the status's state, never the job's")
	assert.Contains(t, cfg, "\n          exit 0", "the job ends clean once the status is written")
}
