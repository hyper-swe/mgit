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
