package packaging

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// composeBlock is the verdict step's status composition, cut from the
// workflow between its markers so the test runs the very lines CI runs.
func composeBlock(t *testing.T) string {
	t.Helper()
	cfg := readRepoFile(t, ".github/workflows/verdict.yml")
	const begin, end = "# --- compose the status", "# --- end compose"
	i, j := strings.Index(cfg, begin), strings.Index(cfg, end)
	if i < 0 || j < i {
		t.Fatalf("verdict.yml must mark its status composition with %q … %q", begin, end)
	}
	return cfg[i:j]
}

// A LISTED WORD IN THE PULL REQUEST'S TEXT FAILS THE VERDICT GATE (MGIT-242).
// The text check's verdict outranks the reviewer's: a PASS at head never
// greens text that carries a listed word, and a text check that could not
// read is red too. Each row runs the workflow's own composition lines with
// the two checks' exit codes and output. Refs: MGIT-242
func TestVerdictGate_ATextHitFailsTheGate(t *testing.T) {
	const verdictPass = "verdictgate: PASS at 8a29f48 names the current head — green" //nolint:gosec // G101: a verdict line, not a credential
	tests := []struct {
		name, tout, out string
		tcode, code     int
		state, desc     string
	}{
		{"text clean, PASS at head", "prtext: PASS — no listed word", verdictPass, 0, 0, "success", "verdictgate: PASS"},
		{"a listed word, PASS at head", "prtext: FAIL — 1 text versions carry a listed word", verdictPass, 1, 0, "failure", "prtext: FAIL"},
		{"text not checked, PASS at head", "prtext: NOT CHECKED — #1: boom", verdictPass, 2, 0, "failure", "prtext: NOT CHECKED"},
		{"text check silent, PASS at head", "", verdictPass, 2, 0, "failure", "prtext: NOT CHECKED"},
		{"text clean, no verdict", "prtext: PASS — no listed word", "verdictgate: NOT CHECKED — no verdict", 0, 1, "failure", "verdictgate: NOT CHECKED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := fmt.Sprintf("tout=%q; tcode=%d; out=%q; code=%d\n%s\nprintf '%%s|%%s' \"$state\" \"$desc\"",
				tt.tout, tt.tcode, tt.out, tt.code, composeBlock(t))
			got, err := exec.CommandContext(context.Background(), "bash", "-c", script).CombinedOutput() //nolint:gosec // G204: the script is the workflow file's own composition block and this table's literals
			require.NoError(t, err, "%s", got)
			state, desc, _ := strings.Cut(string(got), "|")
			assert.Equal(t, tt.state, state)
			assert.True(t, strings.HasPrefix(desc, tt.desc), "description %q", desc)
			assert.LessOrEqual(t, len(desc), 140, "a commit status description holds 140 characters")
		})
	}
}

// The text changes without a push: a title or description edit, a review, a
// review comment. Each must re-run the gate, and runs for one pull request
// queue in order, so the last status written reads the latest text.
// Refs: MGIT-242
func TestVerdictGate_RunsOnEveryTextEvent(t *testing.T) {
	cfg := readRepoFile(t, ".github/workflows/verdict.yml")
	for _, want := range []string{
		"types: [opened, synchronize, reopened, edited]",
		"pull_request_review:", "pull_request_review_comment:",
		"./scripts/prtext", "concurrency:", "cancel-in-progress: false",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("verdict.yml lacks %q", want)
		}
	}
}
