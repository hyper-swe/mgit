package packaging

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// LIBKRUNFW IS A SEPARATE, OPT-IN DOWNLOAD, NEVER PART OF MGIT'S ARCHIVE.
//
// The macOS sandbox needs libkrunfw, a library that carries a Linux kernel
// under LGPL-2.1-only and GPL-2.0-only. mgit's macOS archive does not carry
// it. install.sh fetches it on its own only when the user enables the sandbox
// (MGIT_SANDBOX=1) and accepts a prompt that names the licences and the
// source, verifies its checksum, and installs it beside libkrun in
// <prefix>/lib/mgit with its notices. Refs: MGIT-306

const libkrunfwTestVersion = "5.5.0"

// libkrunfwNotices are the files the opt-in tarball carries beside the dylib.
var libkrunfwNotices = []string{"LICENSE-GPL-2.0-only", "LICENSE-LGPL-2.1-only", "NOTICE.txt", "SOURCE-OFFER.txt"}

// fakeLibkrunfwDownload writes the opt-in download's location: the tarball
// and a checksums.txt naming it. files maps tarball paths to contents.
func fakeLibkrunfwDownload(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	name := "libkrunfw_" + libkrunfwTestVersion + "_darwin_arm64.tar.gz"
	f, err := os.Create(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for path, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	//nolint:gosec // G703: dir is this test's own t.TempDir and the name a constant
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checksums.txt"),
		[]byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o600))
	return dir
}

// completeLibkrunfwDownload is a download carrying the dylib and every notice.
func completeLibkrunfwDownload(t *testing.T) string {
	t.Helper()
	files := map[string]string{"libkrunfw.5.dylib": "krunfw"}
	for _, n := range libkrunfwNotices {
		files[n] = n
	}
	return fakeLibkrunfwDownload(t, files)
}

// macOSRelease is a darwin/arm64 mgit release with the daemon and libkrun,
// and no libkrunfw: the archive as the release ships it.
func macOSRelease(t *testing.T) string {
	t.Helper()
	return fakeReleaseFor(t, "9.9.9", "darwin", "arm64", map[string]string{
		"mgit.sh-exec":                "#!/bin/sh\necho 'mgit version 9.9.9'\n",
		"mgit-sandboxd.sh-exec":       "#!/bin/sh\necho 'mgit-sandboxd version 9.9.9'\n",
		"lib/libkrun.1.dylib":         "krun",
		"THIRD_PARTY/libkrun-LICENSE": "apache",
	})
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
}

func TestInstallScript_MacOS_SandboxNotEnabled_FetchesNoLibkrunfw(t *testing.T) {
	skipOnWindows(t)
	prefix := t.TempDir()
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_LIBKRUNFW_BASE=file://"+completeLibkrunfwDownload(t))
	require.NoError(t, err, "%s", out)
	assert.FileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrun.1.dylib"))
	assert.NoFileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"),
		"libkrunfw is fetched only when the user enables the sandbox:\n%s", out)
	assert.Contains(t, out, "MGIT_SANDBOX=1", "the installer must say how to enable the sandbox step")
}

func TestInstallScript_MacOS_SandboxEnabledAndAccepted_InstallsLibkrunfwBesideLibkrun(t *testing.T) {
	skipOnWindows(t)
	prefix := t.TempDir()
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes",
		"MGIT_LIBKRUNFW_BASE=file://"+completeLibkrunfwDownload(t))
	require.NoError(t, err, "%s", out)

	assert.FileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"), "%s", out)
	assert.FileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrun.1.dylib"))
	for _, n := range libkrunfwNotices {
		assert.FileExists(t, filepath.Join(prefix, "share", "mgit", "THIRD_PARTY", "libkrunfw", n), "%s", out)
	}
	assert.NoFileExists(t, filepath.Join(prefix, "bin", "libkrunfw.5.dylib"), "libraries never land on PATH")

	// The notice comes BEFORE the download, and names what the user accepts.
	notice := strings.Index(out, "LGPL-2.1-only AND GPL-2.0-only")
	fetch := strings.Index(out, "downloading libkrunfw")
	require.NotEqual(t, -1, notice, "the licences must be named:\n%s", out)
	require.NotEqual(t, -1, fetch, "the download must be announced:\n%s", out)
	assert.Less(t, notice, fetch, "the licence notice must come before the download")
	for _, want := range []string{
		"libkrunfw " + libkrunfwTestVersion,
		"https://github.com/containers/libkrunfw/releases/tag/v" + libkrunfwTestVersion,
		"https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.91.tar.xz",
	} {
		assert.Contains(t, out[:fetch], want, "the notice before the download must name %q", want)
	}
}

func TestInstallScript_MacOS_LibkrunfwDeclined_DownloadsNothing(t *testing.T) {
	skipOnWindows(t)
	prefix := t.TempDir()
	// A location that does not exist: any fetch attempt would fail loudly.
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=no",
		"MGIT_LIBKRUNFW_BASE=file://"+filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "declining is a choice, not a failure:\n%s", out)
	assert.Contains(t, out, "LGPL-2.1-only AND GPL-2.0-only", "the notice is shown before the question")
	assert.NotContains(t, out, "downloading libkrunfw")
	assert.NoFileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"))
	assert.FileExists(t, filepath.Join(prefix, "bin", "mgit"), "core mgit is installed either way")
}

func TestInstallScript_MacOS_LibkrunfwChecksumMismatch_InstallsNoLibkrunfw(t *testing.T) {
	skipOnWindows(t)
	download := completeLibkrunfwDownload(t)
	require.NoError(t, os.WriteFile(filepath.Join(download, "checksums.txt"),
		[]byte(strings.Repeat("0", 64)+"  libkrunfw_"+libkrunfwTestVersion+"_darwin_arm64.tar.gz\n"), 0o600))
	prefix := t.TempDir()
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes", "MGIT_LIBKRUNFW_BASE=file://"+download)
	require.Error(t, err, "%s", out)
	assert.Contains(t, out, "checksum mismatch")
	assert.NoFileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"))
}

func TestInstallScript_MacOS_LibkrunfwMissingANotice_InstallsNoLibkrunfw(t *testing.T) {
	skipOnWindows(t)
	download := fakeLibkrunfwDownload(t, map[string]string{
		"libkrunfw.5.dylib": "krunfw", "LICENSE-GPL-2.0-only": "gpl", "LICENSE-LGPL-2.1-only": "lgpl", "NOTICE.txt": "n",
	})
	prefix := t.TempDir()
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes", "MGIT_LIBKRUNFW_BASE=file://"+download)
	require.Error(t, err, "%s", out)
	assert.Contains(t, out, "SOURCE-OFFER.txt")
	assert.NoFileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.5.dylib"),
		"the library never ships without its licence texts and source offer")
}

// Where the download is hosted is not decided yet, so the installer has no
// default location. It must say so instead of guessing a URL.
func TestInstallScript_MacOS_NoLibkrunfwLocation_SaysSoAndFetchesNothing(t *testing.T) {
	skipOnWindows(t)
	prefix := t.TempDir()
	out, err := runInstall(t, macOSRelease(t), prefix, onMacOS(t),
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes", "MGIT_LIBKRUNFW_BASE=")
	require.Error(t, err, "%s", out)
	assert.Contains(t, out, "MGIT_LIBKRUNFW_BASE")
	assert.Contains(t, out, "brew trust hyper-swe/tap", "the Homebrew path must be named as the other way")
	assert.NotContains(t, out, "downloading libkrunfw")
	assert.FileExists(t, filepath.Join(prefix, "bin", "mgit"), "core mgit is installed either way")
}

// The Linux archive carries its own libkrunfw, so enabling the sandbox there
// fetches nothing more.
func TestInstallScript_Linux_SandboxEnabled_FetchesNoSeparateLibkrunfw(t *testing.T) {
	skipOnWindows(t)
	if runtime.GOOS != "linux" {
		t.Skip("the Linux archive's daemon check runs the daemon, which a fake can do only on a Linux host here")
	}
	release := fakeReleaseFor(t, "9.9.9", "linux", runtime.GOARCH, map[string]string{
		"mgit.sh-exec":          "#!/bin/sh\necho 'mgit version 9.9.9'\n",
		"mgit-sandboxd.sh-exec": "#!/bin/sh\necho 'mgit-sandboxd version 9.9.9'\n",
		"lib/libkrun.so.1":      "krun",
		"lib/libkrunfw.so.5":    "krunfw",
	})
	prefix := t.TempDir()
	out, err := runInstall(t, release, prefix,
		"MGIT_SANDBOX=1", "MGIT_LIBKRUNFW_ACCEPT=yes",
		"MGIT_LIBKRUNFW_BASE=file://"+filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "%s", out)
	assert.NotContains(t, out, "downloading libkrunfw")
	assert.FileExists(t, filepath.Join(prefix, "lib", "mgit", "libkrunfw.so.5"))
}

// The release's own verifier refuses a macOS archive layout that carries
// libkrunfw, so the library cannot reach mgit's archive by accident. The
// refusal comes before any macOS-only tool runs, so it is checked here.
func TestVerifyDarwinSandboxd_AnArchiveCarryingLibkrunfw_IsRefused(t *testing.T) {
	skipOnWindows(t)
	tests := []struct {
		name    string
		krunfw  bool
		refused bool
	}{
		{name: "archive_with_libkrunfw", krunfw: true, refused: true},
		{name: "archive_without_libkrunfw_negative_control", krunfw: false, refused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o750))
			//nolint:gosec // G306: the fake daemon must be executable
			require.NoError(t, os.WriteFile(filepath.Join(dir, "mgit-sandboxd"), []byte("#!/bin/sh\n"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "libkrun.1.dylib"), []byte("krun"), 0o600))
			if tt.krunfw {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "libkrunfw.5.dylib"), []byte("krunfw"), 0o600))
			}
			//nolint:gosec // G204: a fixed script in this repository, a t.TempDir argument
			raw, _ := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release", "verify-darwin-sandboxd.sh"), dir).CombinedOutput()
			out := string(raw)
			if tt.refused {
				assert.Contains(t, out, "carries libkrunfw", "%s", out)
			} else {
				assert.NotContains(t, out, "carries libkrunfw", "%s", out)
			}
		})
	}
}
