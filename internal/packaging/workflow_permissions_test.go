package packaging

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A CALLED WORKFLOW'S JOBS MAY USE NO MORE THAN THE CALL GRANTS, and GitHub
// checks that for EVERY job of the callee when the run starts, whatever the
// job's `if:` says. So a job that only ever runs on schedule can still stop a
// release from starting. e2e.yml's schedule-age asked for statuses:write and
// actions:read. release.yml calls e2e.yml as its gate under contents:write
// and id-token:write. A dispatch of release.yml then concluded startup_failure
// with no job run: "The nested job 'schedule-age' is requesting 'actions:
// read, statuses: write', but is only allowed 'actions: none, statuses:
// none'". The next tag would have failed the same way, with nothing built and
// its number burned. The call sites are WALKED from the tree, so a new one is
// checked without anyone adding it here. Refs: MGIT-244
func TestWorkflows_ACalledWorkflowAsksForNoMoreThanItsCallerGrants(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	var calls, over []string
	for _, f := range files {
		caller := parseWorkflowPerms(t, filepath.Base(f), readRepoFile(t, filepath.Join(".github", "workflows", filepath.Base(f))))
		for _, job := range caller.jobs {
			if job.uses == "" {
				continue
			}
			site := caller.name + "/" + job.id
			calls = append(calls, site)
			grant := job.perms
			if grant == nil {
				grant = caller.top
			}
			// Unstated means the repository's default token, which this
			// file cannot see: CANNOT TELL, said out loud, never a pass.
			require.NotNil(t, grant, "%s calls %s without stating a grant, at the job or the top", site, job.uses)
			require.True(t, strings.HasPrefix(job.uses, "./.github/workflows/"),
				"%s calls %s: only a reusable workflow in this repository can be read and checked here", site, job.uses)
			callee := parseWorkflowPerms(t, job.uses, readRepoFile(t, strings.TrimPrefix(job.uses, "./")))
			over = append(over, askedBeyondGrant(t, site, callee, grant)...)
		}
	}
	assert.Contains(t, calls, "release.yml/e2e", "the walk must find the release's own gate, or it checks nothing")
	sort.Strings(over)
	assert.Empty(t, over, "a called workflow's job asks for more than its call site grants, so the CALLER cannot start")
}

// askedBeyondGrant lists every scope a job of the callee asks for above what
// the call site grants. A job with no permissions of its own asks for the
// callee's top-level ones.
func askedBeyondGrant(t *testing.T, site string, callee workflowPerms, grant map[string]string) []string {
	t.Helper()
	var over []string
	for _, cj := range callee.jobs {
		ask := cj.perms
		if ask == nil {
			ask = callee.top
		}
		for scope, level := range ask {
			if permLevel(t, level) > permLevel(t, grant[scope]) {
				over = append(over, fmt.Sprintf("%s → %s job %s asks %s: %s, the call grants %s",
					site, callee.name, cj.id, scope, level, orNone(grant[scope])))
			}
		}
	}
	return over
}

// THE RELEASE'S GRANT IS WRITTEN DOWN WITH ITS REASONS. MGIT-244 could have
// been fixed in one line, by granting the gate statuses:write, and that would
// have handed the publish path's token a scope for a job that never runs
// there. So the grant is pinned here: a scope added to release.yml, at the top
// or on any job, fails until its reason is written beside these. Refs: MGIT-244
var releaseGrant = map[string]struct{ level, why string }{
	"contents": {"write", "goreleaser creates the GitHub release and uploads the archives, checksums and source assets to it"},
	"id-token": {"write", "cosign signs checksums.txt keyless, with this workflow's OIDC identity"},
}

func TestRelease_GrantsOnlyScopesWithAStatedReason(t *testing.T) {
	wf := parseWorkflowPerms(t, "release.yml", readRepoFile(t, filepath.Join(".github", "workflows", "release.yml")))
	require.NotEmpty(t, wf.top, "release.yml states its grant at the top, and an empty read would check nothing")
	grants := map[string]map[string]string{"(top)": wf.top}
	for _, j := range wf.jobs {
		if j.perms != nil {
			grants[j.id] = j.perms
		}
	}
	for where, g := range grants {
		for scope, level := range g {
			want, ok := releaseGrant[scope]
			if !assert.True(t, ok, "release.yml %s grants %s: %s with no stated reason", where, scope, level) {
				continue
			}
			assert.LessOrEqual(t, permLevel(t, level), permLevel(t, want.level),
				"release.yml %s grants %s: %s, above the %s its reason needs (%s)", where, scope, level, want.level, want.why)
		}
	}
}

// The reader above is this file's own, so it is held to the shapes GitHub
// accepts before any verdict rests on it: a block at the top and on a job, a
// `{}` block, a job-level `uses:`, and the step-level `uses:` and comments it
// must NOT take for either. Refs: MGIT-244
func TestParseWorkflowPerms_ReadsTheShapesGitHubAccepts(t *testing.T) {
	wf := parseWorkflowPerms(t, "fixture.yml", `name: x
permissions:
  contents: write # why
  id-token: write

jobs:
  gate:
    uses: ./.github/workflows/e2e.yml
  plain:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      # permissions: none, only a comment
  status:
    permissions:
      statuses: write
      actions: read # why
    runs-on: ubuntu-latest
  closed:
    permissions: {}
    runs-on: ubuntu-latest
`)
	assert.Equal(t, map[string]string{"contents": "write", "id-token": "write"}, wf.top)
	require.Len(t, wf.jobs, 4)
	assert.Equal(t, jobPerms{id: "gate", uses: "./.github/workflows/e2e.yml"}, wf.jobs[0])
	assert.Equal(t, jobPerms{id: "plain"}, wf.jobs[1], "a step's uses and a comment are not the job's")
	assert.Equal(t, map[string]string{"statuses": "write", "actions": "read"}, wf.jobs[2].perms)
	assert.Equal(t, map[string]string{}, wf.jobs[3].perms, "{} grants nothing, which is not the same as unstated")
}

// workflowPerms is what a workflow file grants and asks for. A nil map means
// the file does not say, which is not the same as granting nothing.
type workflowPerms struct {
	name string
	top  map[string]string
	jobs []jobPerms
}

type jobPerms struct {
	id    string
	uses  string // a job-level reusable-workflow call, "" for an ordinary job
	perms map[string]string
}

var (
	jobKey    = regexp.MustCompile(`^([A-Za-z0-9_-]+):\s*$`)
	permEntry = regexp.MustCompile(`^([a-z-]+):\s*([a-z]+)\s*(#.*)?$`)
)

// parseWorkflowPerms reads the permissions blocks of one workflow: the
// top-level block, and each job's own block and `uses:` at job level. YAML
// libraries are not approved here (APPROVED-PACKAGES.md), and the shapes are
// fixed by GitHub's syntax, so this reads lines by indentation. Any form it
// does not read is a failure, never a skip.
func parseWorkflowPerms(t *testing.T, name, text string) workflowPerms {
	t.Helper()
	wf := workflowPerms{name: name}
	var section string
	var job *jobPerms
	var into map[string]string
	intoIndent := -1
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if into != nil && indent >= intoIndent {
			require.Equal(t, intoIndent, indent, "%s:%d: a permission entry nested deeper than its block", name, n+1)
			m := permEntry.FindStringSubmatch(trimmed)
			require.NotNil(t, m, "%s:%d: %q is not a `scope: level` entry", name, n+1, trimmed)
			into[m[1]] = m[2]
			permLevel(t, m[2])
			continue
		}
		into = nil
		switch {
		case indent == 0:
			section = strings.SplitN(trimmed, ":", 2)[0]
			if section == "permissions" {
				wf.top, into = permBlock(t, name, n+1, trimmed)
				intoIndent = 2
			}
		case section == "jobs" && indent == 2 && jobKey.MatchString(trimmed):
			wf.jobs = append(wf.jobs, jobPerms{id: jobKey.FindStringSubmatch(trimmed)[1]})
			job = &wf.jobs[len(wf.jobs)-1]
		case section == "jobs" && indent == 4 && job != nil && strings.HasPrefix(trimmed, "permissions:"):
			job.perms, into = permBlock(t, name, n+1, trimmed)
			intoIndent = 6
		case section == "jobs" && indent == 4 && job != nil && strings.HasPrefix(trimmed, "uses:"):
			job.uses = strings.TrimSpace(strings.TrimPrefix(trimmed, "uses:"))
		}
	}
	return wf
}

// permBlock opens a `permissions:` key, returning the block's map and, for
// the mapping form, the same map to read its entries into (nil for `{}`,
// which has none). Only those two forms are read; a shorthand such as
// read-all would otherwise pass unread.
func permBlock(t *testing.T, name string, line int, key string) (block, into map[string]string) {
	t.Helper()
	inline := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(key, "permissions:"), "#", 2)[0])
	require.Contains(t, []string{"", "{}"}, inline,
		"%s:%d: permissions %q: only the mapping form is read here, and a shorthand must not pass unread", name, line, inline)
	block = map[string]string{}
	if inline == "" {
		into = block
	}
	return block, into
}

// permLevel orders GitHub's permission levels. A scope a block does not name
// is none.
func permLevel(t *testing.T, level string) int {
	t.Helper()
	switch level {
	case "", "none":
		return 0
	case "read":
		return 1
	case "write":
		return 2
	}
	t.Fatalf("unknown permission level %q", level)
	return 0
}

func orNone(level string) string {
	if level == "" {
		return "none"
	}
	return level
}
