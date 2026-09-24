package packaging

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRelease writes a release directory install.sh can install from, laid
// out as goreleaser publishes it: one archive for this host's os/arch and a
// checksums.txt naming it. files maps archive paths to contents; entries
// ending in ".sh-exec" are written executable under the name without it.
func fakeRelease(t *testing.T, version string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	name := fmt.Sprintf("mgit_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	f, err := os.Create(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for path, body := range files {
		mode := int64(0o644)
		if filepath.Ext(path) == ".sh-exec" {
			path, mode = path[:len(path)-len(".sh-exec")], 0o755
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: path, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir path
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checksums.txt"),
		[]byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o600))
	return dir
}

// runInstall runs the repository's install.sh against a local release, into
// prefix, and returns its combined output.
func runInstall(t *testing.T, release, prefix string) (string, error) {
	t.Helper()
	//nolint:gosec // G204: a fixed script path; the env points at t.TempDir paths
	cmd := exec.Command("sh", filepath.Join(repoRoot(t), "install.sh"))
	cmd.Env = append(os.Environ(),
		"MGIT_VERSION=v9.9.9",
		"MGIT_DOWNLOAD_BASE=file://"+release,
		"MGIT_PREFIX="+prefix)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The Linux archive carries the daemon's libraries in lib/ and their
// license texts in THIRD_PARTY/. install.sh puts binaries in <prefix>/bin,
// so the libraries go to <prefix>/lib/mgit — the directory the daemon's run
// path names ($ORIGIN/../lib/mgit) — and the license texts travel with them.
// Without this the README's recommended install leaves a daemon that cannot
// load. Refs: MGIT-230.1, MGIT-229
func TestInstallScript_InstallsTheBundledLibrariesWhereTheDaemonLooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
	release := fakeRelease(t, "9.9.9", map[string]string{
		"mgit.sh-exec":                               "#!/bin/sh\necho 'mgit version 9.9.9 (commit: abc, built: now)'\n",
		"mgit-sandboxd.sh-exec":                      "#!/bin/sh\necho 'mgit-sandboxd version 9.9.9 (commit: abc, built: now)'\n",
		"lib/libkrun.so.1":                           "krun",
		"lib/libkrunfw.so.5":                         "krunfw",
		"THIRD_PARTY/SOURCES.txt":                    "sources",
		"THIRD_PARTY/libkrunfw-LICENSE-GPL-2.0-only": "gpl",
		"guest/mgit.sh-exec":                         "#!/bin/sh\n",
		"guest/mgit-guest.sh-exec":                   "#!/bin/sh\n",
	})
	prefix := t.TempDir()
	out, err := runInstall(t, release, prefix)
	require.NoError(t, err, "%s", out)

	for _, want := range []string{
		"bin/mgit", "bin/mgit-sandboxd",
		"lib/mgit/libkrun.so.1", "lib/mgit/libkrunfw.so.5",
		"share/mgit/THIRD_PARTY/SOURCES.txt", "share/mgit/THIRD_PARTY/libkrunfw-LICENSE-GPL-2.0-only",
		"libexec/guest/mgit", "libexec/guest/mgit-guest",
	} {
		assert.FileExists(t, filepath.Join(prefix, want), "install.sh output:\n%s", out)
	}
	assert.NoFileExists(t, filepath.Join(prefix, "bin", "libkrun.so.1"), "libraries never land on PATH")
}

// An archive without a bundle (macOS, Windows, and every release before the
// bundle) installs exactly as before: no lib/mgit, no share/mgit.
func TestInstallScript_AnArchiveWithoutABundle_InstallsNoLibraries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
	release := fakeRelease(t, "9.9.9", map[string]string{
		"mgit.sh-exec": "#!/bin/sh\necho 'mgit version 9.9.9'\n",
	})
	prefix := t.TempDir()
	out, err := runInstall(t, release, prefix)
	require.NoError(t, err, "%s", out)
	assert.FileExists(t, filepath.Join(prefix, "bin", "mgit"))
	assert.NoDirExists(t, filepath.Join(prefix, "lib", "mgit"))
	assert.NoDirExists(t, filepath.Join(prefix, "share", "mgit"))
}

// The download base is overridable (a mirror, an air-gapped copy, this
// test) but the checksum is still verified against what was fetched.
func TestInstallScript_AMirrorIsStillChecksumVerified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX sh")
	}
	release := fakeRelease(t, "9.9.9", map[string]string{"mgit.sh-exec": "#!/bin/sh\necho ok\n"})
	require.NoError(t, os.WriteFile(filepath.Join(release, "checksums.txt"),
		[]byte("0000000000000000000000000000000000000000000000000000000000000000  "+
			fmt.Sprintf("mgit_9.9.9_%s_%s.tar.gz\n", runtime.GOOS, runtime.GOARCH)), 0o600))
	out, err := runInstall(t, release, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, out, "checksum mismatch")
}
