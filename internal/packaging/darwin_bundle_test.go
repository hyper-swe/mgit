package packaging

// Carry a patched libkrun in the macOS build; fixes MGIT-225.
// Refs: MGIT-259, MGIT-225

import (
	"crypto/sha256"
	"encoding/hex"
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

// pinValue returns KEY's value from a KEY=VALUE file such as pins.env.
func pinValue(t *testing.T, pins, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(\S+)$`).FindStringSubmatch(pins)
	require.NotNil(t, m, "pins.env defines no %s", key)
	return strings.Trim(m[1], `"`)
}

func TestGoreleaser_DarwinSandboxdIsThePrebuiltBundle(t *testing.T) {
	block := yamlBlock(t, readRepoFile(t, ".goreleaser.yaml"), "- id: mgit-sandboxd-darwin")
	assert.Contains(t, block, "tool: ./scripts/release/gobinary-prebuilt.sh",
		"the darwin daemon must come from scripts/release/build-darwin-sandboxd.sh, never a compile here")
	for _, not := range []string{"/opt/homebrew", "PKG_CONFIG_PATH", "CGO_ENABLED", "hooks:"} {
		assert.NotContains(t, block, not, "the darwin build must not carry %q", not)
	}
	assert.Regexp(t, `goos:\s*\n\s*- darwin\s*\n\s*goarch:\s*\n\s*- arm64\s*$`, block, "darwin/arm64 only")
}

func TestGoreleaser_DarwinArchiveCarriesTheBundle(t *testing.T) {
	archives := topLevelBlock(t, readRepoFile(t, ".goreleaser.yaml"), "archives")
	for _, want := range []string{
		`src: "dist/mgit-sandboxd-darwin_{{ .Os }}_{{ .Arch }}*/lib/*"`,
		`src: "dist/mgit-sandboxd-darwin_{{ .Os }}_{{ .Arch }}*/THIRD_PARTY/*"`,
	} {
		assert.Contains(t, archives, want)
	}
}

func TestLibkrunDarwinPatch_IsPinnedByDigest(t *testing.T) {
	pins := readRepoFile(t, "scripts/sandbox-image/pins.env")
	name := pinValue(t, pins, "LIBKRUN_DARWIN_PATCH")
	want := pinValue(t, pins, "LIBKRUN_DARWIN_PATCH_SHA256")
	assert.Equal(t, filepath.Base(name), name, "the patch is named relative to scripts/sandbox-image")

	body := readRepoFile(t, filepath.Join("scripts", "sandbox-image", name))
	sum := sha256.Sum256([]byte(body))
	assert.Equal(t, want, hex.EncodeToString(sum[:]), "LIBKRUN_DARWIN_PATCH_SHA256 must be the patch's digest")

	header, _, _ := strings.Cut(body, "\n")
	assert.Equal(t, "MGIT-225", header, "a one-line header")

	files := regexp.MustCompile(`(?m)^diff --git a/(\S+) b/(\S+)$`).FindAllStringSubmatch(body, -1)
	require.NotEmpty(t, files, "the patch changes no file")
	for _, f := range files {
		assert.Equal(t, f[1], f[2])
		assert.True(t, strings.HasPrefix(f[1], "src/devices/src/virtio/fs/macos/"),
			"%s: the patch changes macOS code only", f[1])
	}
}

func TestApplyLibkrunPatch_SelfTest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release scripts are bash")
	}
	//nolint:gosec // G204: a fixed repo-relative script path; no input reaches the argv
	cmd := exec.Command("bash", filepath.Join("scripts", "release", "apply-libkrun-patch-selftest.sh"))
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Contains(t, string(out), "apply-libkrun-patch selftest: PASS")
}

func TestDarwinAssembler_BuildsThePatchedLibkrunAndVerifiesItsOutput(t *testing.T) {
	script := readRepoFile(t, "scripts/release/build-darwin-sandboxd.sh")
	for _, want := range []string{
		"scripts/sandbox-image/pins.env",
		`apply-libkrun-patch.sh" "$src" "$here/../sandbox-image/$LIBKRUN_DARWIN_PATCH" "$LIBKRUN_DARWIN_PATCH_SHA256"`,
		"NET=1",
		"TIMESYNC=1",
		"install_name_tool -id @rpath/libkrun.1.dylib",
		"-Wl,-rpath,@executable_path/lib",
		"-Wl,-rpath,@executable_path/../lib/mgit",
		"--entitlements \"$root/build/darwin/vz.entitlements\"",
		`verify-darwin-sandboxd.sh" "$out"`,
		"MANIFEST.sha256",
	} {
		assert.Contains(t, script, want, "build-darwin-sandboxd.sh must carry %q", want)
	}
	for _, rel := range []string{
		"scripts/release/build-darwin-sandboxd.sh",
		"scripts/release/verify-darwin-sandboxd.sh",
		"scripts/release/apply-libkrun-patch.sh",
	} {
		info, err := os.Stat(filepath.Join(repoRoot(t), rel))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode().Perm()&0o111, "%s must be executable", rel)
	}
}

func TestE2EWorkflow_BuildsTheDarwinDaemonOnMacOS(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "darwin-sandboxd")
	for _, want := range []string{
		"runs-on: macos-14",
		"fetch-depth: 0",
		`v="${GITHUB_REF_NAME#v}"`,
		"scripts/release/build-darwin-sandboxd.sh",
		`"$RUNNER_TEMP/prebuilt/darwin_arm64"`,
		"name: darwin-sandboxd-arm64",
		"if-no-files-found: error",
	} {
		assert.Contains(t, job, want, "the darwin-sandboxd job must carry %q", want)
	}
}

func TestReleaseWorkflow_ShipsTheGatesDarwinDaemon(t *testing.T) {
	release := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "release")
	for _, want := range []string{
		"pattern: darwin-sandboxd-*",
		"MGIT_DARWIN_PREBUILT: ${{ github.workspace }}/dist-prebuilt",
	} {
		assert.Contains(t, release, want, "the release job must carry %q", want)
	}
}

func TestReleaseWorkflow_SmokeRequiresTheBundledLibkrun(t *testing.T) {
	smoke := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "release-smoke")
	assert.Contains(t, smoke, "'libkrun resolves inside the archive'",
		"the post-publish smoke must require the bundle check to have run")
}

// onMacOS returns the PATH entry that makes install.sh's `uname` answer
// Darwin/arm64 on any host.
func onMacOS(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	//nolint:gosec // G306: the fake uname must be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "uname"),
		[]byte("#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo arm64 ;; *) echo Darwin ;; esac\n"), 0o755))
	return "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestInstallScript_MacOSArchive_InstallsTheBundleAndChecksTheDaemonLoads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
	release := fakeReleaseFor(t, "9.9.9", "darwin", "arm64", map[string]string{
		"mgit.sh-exec":                "#!/bin/sh\necho 'mgit version 9.9.9 (commit: abc, built: now)'\n",
		"mgit-sandboxd.sh-exec":       "#!/bin/sh\necho 'mgit-sandboxd version 9.9.9 (commit: abc, built: now)'\n",
		"lib/libkrun.1.dylib":         "krun",
		"THIRD_PARTY/SOURCES.txt":     "sources",
		"THIRD_PARTY/libkrun-LICENSE": "apache",
		"guest/mgit.sh-exec":          "#!/bin/sh\n",
		"guest/mgit-guest.sh-exec":    "#!/bin/sh\n",
	})
	prefix := t.TempDir()
	out, err := runInstall(t, release, prefix, onMacOS(t))
	require.NoError(t, err, "%s", out)
	for _, want := range []string{
		"bin/mgit", "bin/mgit-sandboxd", "lib/mgit/libkrun.1.dylib",
		"share/mgit/THIRD_PARTY/SOURCES.txt", "share/mgit/THIRD_PARTY/libkrun-LICENSE",
	} {
		assert.FileExists(t, filepath.Join(prefix, want), "install.sh output:\n%s", out)
	}
	assert.Contains(t, out, "mgit-sandboxd version 9.9.9", "install.sh must run the installed daemon")
}

func TestInstallScript_MacOSArchive_ADaemonThatDoesNotLoad_FailsTheInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
	release := fakeReleaseFor(t, "9.9.9", "darwin", "arm64", map[string]string{
		"mgit.sh-exec":          "#!/bin/sh\necho 'mgit version 9.9.9'\n",
		"mgit-sandboxd.sh-exec": "#!/bin/sh\necho 'dyld[1]: Library not loaded: @rpath/libkrun.1.dylib' >&2\nexit 134\n",
		"lib/libkrun.1.dylib":   "krun",
	})
	out, err := runInstall(t, release, t.TempDir(), onMacOS(t))
	require.Error(t, err, "%s", out)
	assert.Contains(t, out, "does not load")
	assert.Contains(t, out, "lib/mgit")
}
