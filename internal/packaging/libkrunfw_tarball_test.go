package packaging

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The macOS libkrunfw tarball install.sh downloads is built by a release job
// from upstream's prebuilt kernel image, carries the notices and a source offer
// that names the release holding the source, and is published beside that
// source under the signed checksums. Refs: MGIT-306

func runRepoScript(t *testing.T, root, script string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "release", script)}, args...)...) //nolint:gosec // G204: test-only; a repo script by constant name
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSourceOfferScript_NamesTheCanonicalSourceTheBuildStepsAndTheReleaseTag(t *testing.T) {
	skipOnWindows(t)
	out, err := runRepoScript(t, repoRoot(t), "libkrunfw-source-offer.sh", nil, "v9.9.9")
	require.NoError(t, err, "%s", out)
	for _, want := range []string{
		"https://github.com/libkrun/libkrunfw/releases/tag/v5.5.0",
		"We apply no patches of our own.",
		"sha256 " + libkrunfwPrebuiltSHA256,
		"https://github.com/hyper-swe/mgit/blob/v9.9.9/scripts/release/build-darwin-libkrunfw.sh",
		"https://github.com/hyper-swe/mgit/releases/tag/v9.9.9",
		"libkrunfw-v5.5.0-kernel-linux-6.12.91.tar.xz",
		"libkrunfw-v5.5.0-source.tar.gz",
		"libkrunfw-v5.5.0-prebuilt-aarch64.tgz (object code, for reference)",
		"write to " + libkrunfwSourceContact,
		"open to anyone who has this binary",
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "containers/libkrunfw")
	assert.NotContains(t, out, "TODO")
}

func TestSourceOfferScript_WithoutATag_IsRefused(t *testing.T) {
	skipOnWindows(t)
	out, err := runRepoScript(t, repoRoot(t), "libkrunfw-source-offer.sh", nil)
	require.Error(t, err, "%s", out)
	assert.Contains(t, out, "usage")
}

// prebuiltFixture writes a prebuilt-asset lookalike (libkrunfw/Makefile and the
// two license texts) and returns it with its sha256.
func prebuiltFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "libkrunfw-prebuilt-aarch64.tgz")
	f, err := os.Create(path) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"libkrunfw/Makefile":              "all:\n\t@true\n",
		"libkrunfw/LICENSE-GPL-2.0-only":  "gpl text",
		"libkrunfw/LICENSE-LGPL-2.1-only": "lgpl text",
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

// scriptTree copies the two release scripts beside a pins.env of the test's
// own, so the pin the script checks is the fixture's, and nothing in the real
// scripts is loosened for a test.
func scriptTree(t *testing.T, sha string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts", "release"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts", "sandbox-image"), 0o750))
	for _, s := range []string{"build-darwin-libkrunfw.sh", "libkrunfw-source-offer.sh"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "release", s), []byte(readRepoFile(t, "scripts/release/"+s)), 0o700)) //nolint:gosec // a t.TempDir path
	}
	pins := "LIBKRUNFW_VERSION=v5.5.0\nLIBKRUNFW_KERNEL_VERSION=6.12.91\nLIBKRUNFW_KERNEL_URL=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.91.tar.xz\nLIBKRUNFW_PREBUILT_SHA256=" + sha + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "scripts", "sandbox-image", "pins.env"), []byte(pins), 0o600))
	return root
}

// fakeMake stands in for make: the plain call succeeds, and `install` lays
// down the dylib the real build would.
func fakeMake(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakemake")
	body := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in PREFIX=*) p=\"${a#PREFIX=}\" ;; esac; done\n" +
		"if [ -n \"$p\" ]; then mkdir -p \"$p/lib\" && printf 'dylib' >\"$p/lib/libkrunfw.5.dylib\"; fi\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o700)) //nolint:gosec // a t.TempDir path
	return p
}

func tarballEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck // read-only
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		got[h.Name] = string(b)
	}
}

func TestBuildDarwinLibkrunfw_AssemblesTheTarballInstallShTakes(t *testing.T) {
	skipOnWindows(t)
	prebuilt, sha := prebuiltFixture(t)
	root := scriptTree(t, sha)
	out := t.TempDir()
	log, err := runRepoScript(t, root, "build-darwin-libkrunfw.sh", []string{"MAKE=" + fakeMake(t)}, prebuilt, out, "v9.9.9")
	require.NoError(t, err, "%s", log)

	name := "libkrunfw_" + libkrunfwTestVersion + "_darwin_arm64.tar.gz"
	entries := tarballEntries(t, filepath.Join(out, name))
	for _, n := range []string{"libkrunfw.5.dylib", "LICENSE-GPL-2.0-only", "LICENSE-LGPL-2.1-only", "NOTICE.txt", "SOURCE-OFFER.txt"} {
		assert.Contains(t, entries, n, "the tarball must hold %s at its top level", n)
	}
	assert.Len(t, entries, 5)
	assert.Equal(t, "dylib", entries["libkrunfw.5.dylib"])
	assert.Equal(t, "gpl text", entries["LICENSE-GPL-2.0-only"])
	assert.Contains(t, entries["SOURCE-OFFER.txt"], "https://github.com/hyper-swe/mgit/releases/tag/v9.9.9")
	assert.Contains(t, entries["NOTICE.txt"], "distributed separately from mgit")

	// The installer takes exactly this tarball, under checksums it can verify.
	data, err := os.ReadFile(filepath.Join(out, name)) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	dl := t.TempDir()
	dlRoot, err := os.OpenRoot(dl)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dlRoot.Close() })
	require.NoError(t, dlRoot.WriteFile(name, data, 0o600))
	require.NoError(t, dlRoot.WriteFile("checksums.txt", []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o600))
	prefix := t.TempDir()
	res, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes", "MGIT_LIBKRUNFW_BASE=file://"+dl)
	require.NoError(t, err, "%s", res)
	assert.FileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"))
	assert.FileExists(t, filepath.Join(prefix, "share", "mgit", "THIRD_PARTY", "libkrunfw", "SOURCE-OFFER.txt"))
}

func TestBuildDarwinLibkrunfw_AnAssetThatIsNotThePin_IsRefused(t *testing.T) {
	skipOnWindows(t)
	prebuilt, _ := prebuiltFixture(t)
	root := scriptTree(t, strings.Repeat("0", 64))
	out := t.TempDir()
	log, err := runRepoScript(t, root, "build-darwin-libkrunfw.sh", []string{"MAKE=" + fakeMake(t)}, prebuilt, out, "v9.9.9")
	require.Error(t, err, "%s", log)
	assert.Contains(t, log, "the pin says")
	entries, derr := os.ReadDir(out)
	require.NoError(t, derr)
	assert.Empty(t, entries, "nothing is built from an asset that is not the pin")
}

func TestBuildDarwinLibkrunfw_ABuildThatLeavesNoLibrary_IsRefused(t *testing.T) {
	skipOnWindows(t)
	prebuilt, sha := prebuiltFixture(t)
	root := scriptTree(t, sha)
	nolib := filepath.Join(t.TempDir(), "make")
	require.NoError(t, os.WriteFile(nolib, []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // a t.TempDir path
	out := t.TempDir()
	log, err := runRepoScript(t, root, "build-darwin-libkrunfw.sh", []string{"MAKE=" + nolib}, prebuilt, out, "v9.9.9")
	require.Error(t, err, "%s", log)
	assert.Contains(t, log, "left no")
}

// fakeBrewScript writes a brew that logs every call to calls and answers the
// preflight questions from the preinstalled list.
func fakeBrewScript(t *testing.T, preinstalled string) (brew, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	brew = filepath.Join(dir, "brew")
	body := "#!/bin/sh\necho \"$*\" >>" + calls + "\n" +
		"case \"$1 $2 $3\" in\n" +
		"  \"list --versions " + preinstalled + "\") echo \"" + preinstalled + " 1.0\"; exit 0 ;;\n" +
		"esac\nexit 1\n"
	require.NoError(t, os.WriteFile(brew, []byte(body), 0o700)) //nolint:gosec // a t.TempDir path
	return brew, calls
}

// A preflight refusal must leave what was already installed alone: the check
// has installed nothing, so its cleanup must uninstall nothing.
func TestBrewInstallCheck_ARefusedPreflightUninstallsNothing(t *testing.T) {
	skipOnWindows(t)
	root := repoRoot(t)
	for _, preinstalled := range []string{"libkrun", "libkrunfw"} {
		t.Run(preinstalled, func(t *testing.T) {
			brew, calls := fakeBrewScript(t, preinstalled)
			cmd := exec.Command("bash", filepath.Join(root, "scripts", "brew-install-no-libkrun.sh")) //nolint:gosec // G204: test-only; a repo script by constant name
			cmd.Env = append(os.Environ(), "BREW="+brew)
			out, err := cmd.CombinedOutput()
			require.Error(t, err, "the check must refuse when %s is already installed: %s", preinstalled, out)
			log, readErr := os.ReadFile(calls) //nolint:gosec // a t.TempDir path
			require.NoError(t, readErr)
			assert.NotContains(t, string(log), "uninstall", "a refusal ran an uninstall:\n%s", log)
		})
	}
}

// The assembler owns its scratch directory and leaves none behind, on success
// or on failure.
func TestBuildDarwinLibkrunfw_RemovesItsScratchDirectoryOnSuccessAndOnFailure(t *testing.T) {
	skipOnWindows(t)
	prebuilt, sha := prebuiltFixture(t)
	root := scriptTree(t, sha)
	nolib := filepath.Join(t.TempDir(), "nolib")
	require.NoError(t, os.WriteFile(nolib, []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // a t.TempDir path
	for name, mk := range map[string]string{"success": fakeMake(t), "failure": nolib} {
		t.Run(name, func(t *testing.T) {
			scratch := t.TempDir()
			_, err := runRepoScript(t, root, "build-darwin-libkrunfw.sh", []string{"MAKE=" + mk, "TMPDIR=" + scratch}, prebuilt, t.TempDir(), "v9.9.9")
			if name == "success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			left, readErr := os.ReadDir(scratch)
			require.NoError(t, readErr)
			assert.Empty(t, left, "the script left its scratch directory behind")
		})
	}
}

func TestE2EWorkflow_BuildsAndSmokeTestsTheMacOSLibkrunfwTarballAndUploadsItOnATag(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "darwin-libkrunfw")
	for _, want := range []string{
		"runs-on: macos-15",
		"scripts/release/build-darwin-libkrunfw.sh",
		"scripts/ci/guard-fetch.sh",
		"krunfw_get_version",
		`dlopen("libkrunfw.5.dylib"`,
		"DYLD_FALLBACK_LIBRARY_PATH",
		`want="LICENSE-GPL-2.0-only LICENSE-LGPL-2.1-only NOTICE.txt SOURCE-OFFER.txt libkrunfw.5.dylib"`,
		`the tarball holds:`,
		`python3 -I -m tarfile -l`,
		"name: darwin-libkrunfw",
		"if: github.ref_type == 'tag'",
	} {
		assert.Contains(t, job, want, "the darwin-libkrunfw job must carry %q", want)
	}
}

func TestReleaseWorkflow_PublishesAndRequiresTheMacOSLibkrunfwTarball(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release.yml")
	assert.Contains(t, wf, "name: darwin-libkrunfw", "the release must take the tarball the gate built")
	assert.Contains(t, wf, `"libkrunfw_${LIBKRUNFW_VERSION#v}_darwin_arm64.tar.gz"`,
		"the post-release check must require the tarball in the assets and in checksums.txt")
}
