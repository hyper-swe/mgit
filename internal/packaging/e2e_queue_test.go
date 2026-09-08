package packaging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const e2eWorkflow = ".github/workflows/e2e.yml"

// A push to a pull request cancels the e2e run it supersedes — 34 of the 97
// PR runs in the eight days to 2026-09-08 were overtaken by a newer push
// before they finished, each a full matrix nobody would read. Every other
// event (a push to main, the nightly, a release's call, a dispatch) produces
// evidence and gets a group of its own: a shared group holds one pending run
// and cancels the rest. Refs: MGIT-202
func TestE2E_CancelsOnlySupersededPullRequestRuns(t *testing.T) {
	cfg := readRepoFile(t, e2eWorkflow)
	group, cancel := concurrencySettings(cfg)
	require.NotEmpty(t, group, "%s: no top-level concurrency group", e2eWorkflow)
	assert.Contains(t, group, "github.event.pull_request.number", "pull requests share a group per PR")
	assert.Contains(t, group, "github.run_id", "every other event gets a group of its own")
	assert.Equal(t, "${{ github.event_name == 'pull_request' }}", cancel,
		"cancel-in-progress holds only for pull requests")
}

// Push and pull_request runs skip changes no e2e job reads: documentation,
// the board, license files. The ignore list may name nothing else — a pattern
// reaching code, scripts, workflows or the module files would remove
// evidence, and the two triggers carry the same list. Refs: MGIT-202
func TestE2E_SkipsOnlyChangesNoJobReads(t *testing.T) {
	cfg := readRepoFile(t, e2eWorkflow)
	allowed := map[string]bool{
		`"**/*.md"`: true, `"docs/**"`: true, `".mtix/**"`: true, `"LICENSE"`: true, `".gitignore"`: true,
	}
	push := triggerPathsIgnore(cfg, "push")
	pr := triggerPathsIgnore(cfg, "pull_request")
	require.NotEmpty(t, push, "%s: push carries no paths-ignore", e2eWorkflow)
	assert.Equal(t, push, pr, "push and pull_request ignore the same paths")
	for _, p := range push {
		assert.True(t, allowed[p], "paths-ignore entry %s is not a documentation-or-board pattern", p)
	}
}

// concurrencySettings returns the group and cancel-in-progress values of the
// top-level `concurrency:` block.
func concurrencySettings(cfg string) (group, cancel string) {
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "concurrency:" {
			continue
		}
		for _, next := range lines[i+1:] {
			if len(next) > 0 && next[0] != ' ' {
				break
			}
			trimmed := strings.TrimSpace(next)
			if v, ok := strings.CutPrefix(trimmed, "group:"); ok {
				group = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(trimmed, "cancel-in-progress:"); ok {
				cancel = strings.TrimSpace(v)
			}
		}
	}
	return group, cancel
}

// triggerPathsIgnore returns the `paths-ignore:` entries under the named
// top-level trigger, in file order.
func triggerPathsIgnore(cfg, trigger string) []string {
	lines := strings.Split(cfg, "\n")
	var out []string
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "  "+trigger+":" {
			continue
		}
		inList := false
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(next, "  ") && !strings.HasPrefix(next, "   ") {
				break // the next trigger
			}
			trimmed := strings.TrimSpace(next)
			switch {
			case trimmed == "paths-ignore:":
				inList = true
			case inList && strings.HasPrefix(trimmed, "- "):
				out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			case inList && trimmed != "" && !strings.HasPrefix(trimmed, "#"):
				inList = false
			}
		}
	}
	return out
}
