package packaging

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The release job's checkout is not main's checkout: it also holds the
// directories the workflow downloads the daemons and sources into, and the
// build's own output. A test that walks this repository's working tree met
// a path there that no CI checkout has, and the v0.7.0 release failed in its
// before-hook with the same suite green on main. CI therefore runs the whole
// suite in a workspace shaped like the release job's, built by one committed
// script. These tests keep that script's shape, and the job that uses it,
// pinned to the release workflow, so the shape cannot drift from the job it
// stands in for. Refs: MGIT-281, MGIT-274
const workspaceScript = "scripts/ci/release-workspace-shape.sh"

// releaseDownloadDirs is every directory release.yml downloads artifacts
// into, read from the workflow rather than listed here.
func releaseDownloadDirs(t *testing.T) []string {
	t.Helper()
	wf := readRepoFile(t, ".github/workflows/release.yml")
	var dirs []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+path: (dist[a-z-]*)\s*$`).FindAllStringSubmatch(wf, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			dirs = append(dirs, m[1])
		}
	}
	require.NotEmpty(t, dirs, "release.yml downloads no artifacts into a dist* directory; update this test's source")
	return dirs
}

func TestReleaseWorkspaceShape_NamesEveryDirectoryTheReleaseJobDownloadsInto(t *testing.T) {
	script := readRepoFile(t, workspaceScript)
	for _, dir := range append(releaseDownloadDirs(t), "dist") {
		assert.Contains(t, script, dir+"/", "%s does not create %s, which the release job's checkout holds", workspaceScript, dir)
	}
}

func TestReleaseWorkspaceShape_TheTestJobRunsTheSuiteInIt(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")
	assert.Regexp(t, `(?s)release-shaped:.*bash `+regexp.QuoteMeta(workspaceScript)+`.*go test \./\.\.\. -count=1`, ci,
		"ci.yml has no job that builds the release-shaped workspace and then runs the whole suite")
}
