package packaging

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// golangci-lint sees only files whose build tags match the host it runs on,
// and CI linted once, on ubuntu, without the libkrun tag. Every file built
// only on darwin, or for (darwin || linux && libkrun) such as the libkrun
// real-VM tests, was linted by no job. An unused helper from #173 reached
// main that way; darwin lint names it at once. The macOS libkrun job, which
// already builds and tests that code with a real libkrun, lints it too, at
// the same pinned version as the ubuntu lint. Refs: MGIT-253
func TestCI_TheMacOSLibkrunJobLintsTheFilesUbuntuCannotSee(t *testing.T) {
	ci := readRepoFile(t, filepath.Join(".github", "workflows", "ci.yml"))
	pin := regexp.MustCompile(`golangci/golangci-lint-action@\S+\s+with:\s+version: (\S+)`)
	ubuntu := pin.FindStringSubmatch(jobBlock(t, ci, "test"))
	require.NotNil(t, ubuntu, "the ubuntu test job lints at a pinned version")

	mac := jobBlock(t, ci, "libkrun")
	require.Contains(t, mac, "runs-on: macos-", "the job this pins is the macOS one")
	got := pin.FindStringSubmatch(mac)
	require.NotNil(t, got, "the macOS libkrun job must run golangci-lint")
	assert.Equal(t, ubuntu[1], got[1], "one pinned version for both lint passes")
	lintAt := strings.Index(mac, "golangci/golangci-lint-action@")
	envAt := strings.Index(mac, `PKG_CONFIG_PATH=$(brew --prefix libkrun)/lib/pkgconfig" >> "$GITHUB_ENV"`)
	require.GreaterOrEqual(t, envAt, 0, "the cgo binding needs libkrun's pkg-config path to typecheck")
	assert.Less(t, envAt, lintAt, "and it is set before the lint runs")
}
