package packaging

// The macOS runner image and the deployment target of what it builds.
// Refs: HSWE-17.7.4

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// macos-14 images are retired and browned out; every macOS job runs on the
// pinned supported image instead.
func TestWorkflows_NoMacOS14Runner(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // repo workflow files
		require.NoError(t, err)
		for i, line := range strings.Split(string(b), "\n") {
			assert.NotContainsf(t, line, "macos-14", "%s:%d runs on a retired runner image", filepath.Base(f), i+1)
		}
	}
}

// A binary built on macos-15 would default to a macOS 15 minimum and stop
// running on macOS 14. The build pins the target, keys cgo on it (Go's build
// cache does not key on the environment variable), and verifies the result.
func TestDarwinBuild_PinsTheDeploymentTarget(t *testing.T) {
	pins := readRepoFile(t, "scripts/sandbox-image/pins.env")
	assert.Equal(t, "14.0", pinValue(t, pins, "MACOS_MIN_VERSION"))
	build := readRepoFile(t, "scripts/release/build-darwin-sandboxd.sh")
	for _, want := range []string{
		`MACOSX_DEPLOYMENT_TARGET="$MACOS_MIN_VERSION"`,
		`-mmacosx-version-min=$MACOS_MIN_VERSION`,
		"CGO_CFLAGS", "CGO_LDFLAGS",
		"macho-min-check.sh",
		"newer than",
	} {
		assert.Contains(t, build, want)
	}
	verify := readRepoFile(t, "scripts/release/verify-darwin-sandboxd.sh")
	assert.Contains(t, verify, "macho-min-check.sh", "the verifier checks the shipped files, not only the build")
}

// fakeOtool installs an otool that prints the given load commands for -l.
func fakeOtool(t *testing.T, loadCommands string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + loadCommands + "\nEOF\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "otool"), []byte(script), 0o755)) //nolint:gosec // test stub
	return dir
}

func runMinCheck(t *testing.T, loadCommands string) (string, error) {
	t.Helper()
	bin := fakeOtool(t, loadCommands)
	file := filepath.Join(t.TempDir(), "x")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts/release/macho-min-check.sh"), "14.0", file) //nolint:gosec // repo script
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func buildVersion(minos string) string {
	return "Load command 9\n      cmd LC_BUILD_VERSION\n  cmdsize 32\n platform 1\n    minos " + minos + "\n      sdk 15.2\n"
}

func TestMachoMinCheck(t *testing.T) {
	passes := map[string]string{
		"exactly the minimum": buildVersion("14.0"),
		"older than minimum":  buildVersion("11.0"),
		"old-style version":   "Load command 9\n      cmd LC_VERSION_MIN_MACOSX\n  cmdsize 16\n  version 13.0\n      sdk 14.0\n",
	}
	for name, lc := range passes {
		t.Run("pass/"+name, func(t *testing.T) {
			out, err := runMinCheck(t, lc)
			assert.NoError(t, err, out)
		})
	}
	fails := map[string]string{
		"newer than the minimum": buildVersion("15.0"),
		"patch newer":            buildVersion("14.0.1"),
		"no minimum recorded":    "Load command 1\n      cmd LC_SEGMENT_64\n",
	}
	for name, lc := range fails {
		t.Run("fail/"+name, func(t *testing.T) {
			out, err := runMinCheck(t, lc)
			assert.Error(t, err, out)
		})
	}
}
