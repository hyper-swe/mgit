package packaging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A pull request whose base was not main got no CI at all — both workflows
// triggered on `pull_request: branches: [main]` — so a stacked PR showed zero
// checks and the evidence a merge rule requires lived in hand-dispatched run
// ids nobody running `gh pr checks` would see (MGIT-200). Every pull request
// is checked at its head against its base, whatever the base. Refs: MGIT-200
func TestWorkflows_PullRequestsAreCheckedAgainstEveryBase(t *testing.T) {
	for _, wf := range []string{".github/workflows/ci.yml", ".github/workflows/e2e.yml"} {
		t.Run(wf, func(t *testing.T) {
			cfg := readRepoFile(t, wf)
			assert.False(t, pullRequestFiltersBranches(cfg),
				"%s: `pull_request:` must not carry a `branches:` filter — a PR against a feature branch gets no checks otherwise", wf)
			assert.Contains(t, cfg, "push:\n    branches: [main]", "%s: pushes stay filtered to main", wf)
		})
	}
}

// pullRequestFiltersBranches reports whether the first key under the
// top-level `pull_request:` trigger is `branches:`.
func pullRequestFiltersBranches(cfg string) bool {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "  pull_request:" {
			continue
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			return strings.HasPrefix(next, "    branches:")
		}
	}
	return false
}

// A RETARGETED PULL REQUEST GETS FRESH CI. Whoever merges refreshes a retargeted
// pull request's stale test-merge by moving its base away and back, which is
// a `pull_request: edited` event. With the default event types (opened,
// synchronize, reopened) that refreshed the merge ref and fired NO CI, so a
// current ref sat over an old green. Both workflows now list `edited`, so
// every event that changes what would be merged is checked.
func TestWorkflows_PullRequestsRunOnEdited(t *testing.T) {
	for _, wf := range []string{".github/workflows/ci.yml", ".github/workflows/e2e.yml"} {
		t.Run(wf, func(t *testing.T) {
			types := pullRequestTypes(readRepoFile(t, wf))
			assert.NotEmpty(t, types, "%s: `pull_request:` must list its types explicitly", wf)
			for _, want := range []string{"opened", "synchronize", "reopened", "edited"} {
				assert.Contains(t, types, want, "%s: pull_request types lack %q", wf, want)
			}
		})
	}
}

// pullRequestTypes returns the `types:` line under the top-level
// `pull_request:` trigger, or "" when the trigger relies on the defaults.
func pullRequestTypes(cfg string) string {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "  pull_request:" {
			continue
		}
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "    ") && strings.TrimSpace(next) != "" && !strings.HasPrefix(strings.TrimSpace(next), "#") {
				return ""
			}
			if strings.HasPrefix(next, "    types:") {
				return next
			}
		}
	}
	return ""
}

// THE BRANCH-SCOPE BACKSTOP JUDGES A HEAD AGAINST ITS OWN BASE (MGIT-257).
// It ran `branchguard --base origin/main` for every pull request, so a
// stacked pull request, whose base is another open pull request's branch,
// carried its base's commits as not its own and was red until the base
// merged: measured on #199 at 1cbb294, whose guard passed against its
// declared base. The job passes the pull request's own base, through the
// environment, never interpolated into the script, and main only when an
// event carries no base. Refs: MGIT-257, MGIT-200, MGIT-142
func TestBranchScope_ChecksTheHeadAgainstItsOwnBase(t *testing.T) {
	cfg := readRepoFile(t, ".github/workflows/ci.yml")
	start := strings.Index(cfg, "\n  branch-scope:\n")
	if !assert.GreaterOrEqual(t, start, 0, "ci.yml has the branch-scope job") {
		return
	}
	job := cfg[start+1:]
	if end := strings.Index(job[1:], "\n  # "); end >= 0 {
		job = job[:end+1]
	}
	assert.Contains(t, job, "BASE_REF: ${{ github.base_ref }}", "the base reaches the step through its environment")
	assert.Contains(t, job, `--base "origin/${BASE_REF:-main}"`, "the head is judged against its own base")
	assert.NotContains(t, job, "--base origin/main", "a fixed main base misjudges every stacked pull request")
	for _, line := range strings.Split(job, "\n") {
		if strings.Contains(line, "run:") {
			assert.NotContains(t, line, "${{", "an expression is never interpolated into the script: %s", line)
		}
	}
}
