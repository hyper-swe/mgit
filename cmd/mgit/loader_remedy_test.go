package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runPathDaemon builds a real linux/amd64 ELF carrying runPath as its run
// path, installed at <root>/<rel>, and returns its path. A real binary rather
// than a hand-made header: the remedy's claim is "this is where THIS daemon's
// loader looked", so the test reads it the way the remedy does, out of an ELF
// the Go linker wrote. PIE, because a static binary has no dynamic section.
func runPathDaemon(t *testing.T, root, rel, runPath string) string {
	t.Helper()
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fixture\n\ngo 1.22\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600))
	out := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(out), 0o750))
	args := []string{"build", "-buildmode=pie", "-o", out}
	if runPath != "" {
		args = append(args, "-ldflags=-r "+runPath)
	}
	//nolint:gosec,noctx // G204: a fixed toolchain command building a test fixture
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, "build the fixture daemon: %s", b)
	return out
}

// On Linux the libkrun libraries ship INSIDE the release archive, beside the
// daemon, and nothing is installed from a package manager: a missing
// libkrun.so.1 means the archive was taken apart. The remedy therefore names
// the directories THIS daemon's loader searched, read out of its own run path
// with $ORIGIN expanded, and says to reinstall the archive. The brew wording
// it printed on every platform was wrong here. Refs: MGIT-230.4, MGIT-229
func TestMissingLibraryRemedy_OnLinux_NamesTheRunPathItSearchedAndTheArchive(t *testing.T) {
	root := t.TempDir()
	daemon := runPathDaemon(t, root, filepath.Join("opt", "bin", "mgit-sandboxd"), "$ORIGIN/lib:$ORIGIN/../lib/mgit")
	for _, lib := range []string{"libkrun.so.1", "libkrunfw.so.5"} {
		got := missingLibraryRemedy(lib, daemon, "linux")
		assert.Contains(t, got, lib+" is missing")
		assert.Contains(t, got, filepath.Join(root, "opt", "bin", "lib"), "the first run-path directory, $ORIGIN expanded")
		assert.Contains(t, got, filepath.Join(root, "opt", "lib", "mgit"), "the second, where install.sh puts them")
		assert.NotContains(t, got, "$ORIGIN", "a reader cannot expand it; the remedy does")
		assert.Contains(t, got, "release archive")
		assert.Contains(t, got, "install.sh")
		assert.NotContains(t, got, "brew", "Linux installs nothing from Homebrew")
	}
}

// A run path that cannot be read is said, not guessed: the remedy still names
// the archive, and says why it names no directory.
func TestMissingLibraryRemedy_OnLinux_ARunPathItCannotReadIsSaid(t *testing.T) {
	notELF := filepath.Join(t.TempDir(), "mgit-sandboxd")
	require.NoError(t, os.WriteFile(notELF, []byte("#!/bin/sh\n"), 0o600))
	got := missingLibraryRemedy("libkrun.so.1", notELF, "linux")
	assert.Contains(t, got, "could not read its run path")
	assert.Contains(t, got, "release archive")
	assert.NotContains(t, got, "brew")

	noRunPath := runPathDaemon(t, t.TempDir(), "mgit-sandboxd", "")
	got = missingLibraryRemedy("libkrun.so.1", noRunPath, "linux")
	assert.Contains(t, got, "carries no run path")
	assert.NotContains(t, got, "brew")
}

// The loader's own line says which platform it came from, and so which
// remedy fits: dyld is macOS, where Homebrew is how libkrun arrives, and
// ld.so is Linux, where the archive carries it. The platform is read from
// the words, not from the host running doctor, so the pairing is the same on
// every host this test runs on. Refs: MGIT-230.4
func TestLoaderRemedy_TheLoaderLineChoosesThePlatformsRemedy(t *testing.T) {
	lib, got := loaderRemedy("dyld[95534]: Library not loaded: /opt/homebrew/opt/libkrun/lib/libkrun.1.dylib", "/opt/homebrew/bin/mgit-sandboxd")
	assert.Equal(t, "libkrun.1.dylib", lib)
	assert.Contains(t, got, "brew trust libkrun/krun")

	lib, got = loaderRemedy("mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file", "")
	assert.Equal(t, "libkrun.so.1", lib)
	assert.Contains(t, got, "release archive")
	assert.NotContains(t, got, "brew")

	lib, got = loaderRemedy("panic: something else", "")
	assert.Empty(t, lib)
	assert.Empty(t, got, "no remedy is invented for a cause nobody named")
}

// A host whose glibc is older than the bundle's floor fails at load time with
// a VERSION error, not a missing file, and no reinstall fixes it. It must be
// named as what it is: the floor, and the version the binary asked for. Both
// objects of the bundle can be the one asking. Refs: MGIT-230.4, MGIT-229
func TestLoaderRemedy_AnOldGlibcIsNamedAsThatNotAsAMissingLibrary(t *testing.T) {
	for _, out := range []string{
		"/opt/mgit/bin/mgit-sandboxd: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.30' not found (required by /opt/mgit/bin/mgit-sandboxd)",
		"/opt/mgit/bin/mgit-sandboxd: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.30' not found (required by /opt/mgit/bin/lib/libkrun.so.1)",
	} {
		lib, remedy := loaderRemedy(out, "/opt/mgit/bin/mgit-sandboxd")
		assert.Empty(t, lib, "no library is missing: %s", out)
		assert.Contains(t, remedy, "glibc")
		assert.Contains(t, remedy, "GLIBC_2.30", "the version the binary asked for")
		assert.Contains(t, remedy, linuxGlibcFloor, "the floor the release promises")
		assert.NotContains(t, remedy, "is missing")
		assert.NotContains(t, remedy, "brew")
	}
}

// The floor the remedy states is the one the release verifier enforces, read
// from the verifier rather than restated, so the two cannot drift apart.
func TestLinuxGlibcFloor_IsTheReleaseVerifiersFloor(t *testing.T) {
	// From this file, not the working directory: other tests in the package
	// change directory.
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	//nolint:gosec // G304: a fixed path inside this repository
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "release", "verify-linux-sandboxd.sh"))
	require.NoError(t, err)
	m := regexp.MustCompile(`floor="\$\{3:-([0-9.]+)\}"`).FindStringSubmatch(string(b))
	require.NotNil(t, m, "the verifier states its default floor")
	assert.Equal(t, m[1], linuxGlibcFloor)
	assert.True(t, strings.Count(linuxGlibcFloor, ".") == 1, "a major.minor glibc version")
}
