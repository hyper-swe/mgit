//go:build linux || darwin

package artifactexport

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// recordShareMode stamps the share-metadata record a virtio-fs backend writes
// for a guest-created inode, skipping the test when the host filesystem cannot
// hold the xattr (some Linux tmpfs builds reject the user namespace) — a skip
// is honest there; a failure would be about the filesystem, not the export.
func recordShareMode(t *testing.T, path string, mode uint32) {
	t.Helper()
	value := "0:0:" + formatOctalStat(mode)
	if err := unix.Lsetxattr(path, recordedStatXattr, []byte(value), 0); err != nil {
		t.Skipf("this filesystem cannot hold %s (%v); the share-metadata path "+
			"is measured for real by the libkrun real-VM e2e", recordedStatXattr, err)
	}
}

// formatOctalStat renders a full st_mode the way the backend does.
func formatOctalStat(mode uint32) string {
	const digits = "01234567"
	if mode == 0 {
		return "0"
	}
	var out []byte
	for mode > 0 {
		out = append([]byte{digits[mode&7]}, out...)
		mode >>= 3
	}
	return "0" + string(out)
}

func TestExport_ShareRecordedMode_ReproducesTheModeTheGuestSet(t *testing.T) {
	// The shape libkrun's macOS virtio-fs presents (measured, MGIT-81): the
	// host file is 0600 and the mode the guest set lives in the share record.
	staged := t.TempDir()
	script := filepath.Join(staged, "tree", "bin", "run.sh")
	mustWrite(t, script, "#!/bin/sh\n", 0o600)
	recordShareMode(t, script, syscall.S_IFREG|0o755)
	data := filepath.Join(staged, "tree", "index.js")
	mustWrite(t, data, "module.exports = 1\n", 0o600)
	recordShareMode(t, data, syscall.S_IFREG|0o644)
	_, dest := destIn(t)

	res, err := Export(request(staged, "tree", dest))
	require.NoError(t, err)

	got, err := os.Lstat(filepath.Join(dest, "bin", "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), got.Mode().Perm(),
		"the exported script must carry the mode the guest set, not the share's 0600 placeholder")
	gotData, err := os.Lstat(filepath.Join(dest, "index.js"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o644), gotData.Mode().Perm())

	manifest := readManifest(t, res.ManifestPath)
	for _, e := range manifest.Entries {
		assert.Equal(t, ModeSourceShareRecord, e.ModeSource,
			"%s: the sidecar must attribute the mode to the share record", e.Path)
	}
	assert.Equal(t, "0755", modeOf(t, manifest, "bin/run.sh"))
	assert.Equal(t, "0644", modeOf(t, manifest, "index.js"))
}

func TestExport_NoShareRecord_UsesTheModeTheHostStats(t *testing.T) {
	staged := stagedFixture(t)
	_, dest := destIn(t)

	res, err := Export(request(staged, "node_modules", dest))
	require.NoError(t, err)

	got, err := os.Lstat(filepath.Join(dest, "pkg", "bin", "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), got.Mode().Perm())

	manifest := readManifest(t, res.ManifestPath)
	for _, e := range manifest.Entries {
		assert.Empty(t, e.ModeSource,
			"%s: with no share record the mode is a plain host stat and needs no attribution", e.Path)
	}
}

func TestExport_RestrictiveHostUmask_StillReproducesTheObservedMode(t *testing.T) {
	// An export copies with O_CREATE, whose mode argument the kernel masks with
	// the calling process's umask: a daemon running umask 0077 would otherwise
	// quietly turn an observed 0755 into 0700 and report 0755 in the sidecar.
	//
	// The staged tree is built BEFORE the umask changes, so the mode being
	// observed really is 0755 and the only thing under test is the copy.
	staged := stagedFixture(t)
	_, dest := destIn(t)
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	_, err := Export(request(staged, "node_modules", dest))
	require.NoError(t, err)

	got, err := os.Lstat(filepath.Join(dest, "pkg", "bin", "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), got.Mode().Perm(),
		"the exported file must carry the mode that was observed, not one the umask shaved")
}

// readManifest decodes an export's provenance sidecar.
func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp dir
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// modeOf returns the mode a manifest recorded for one entry.
func modeOf(t *testing.T, m Manifest, path string) string {
	t.Helper()
	for _, e := range m.Entries {
		if e.Path == path {
			return e.Mode
		}
	}
	t.Fatalf("no manifest entry for %q", path)
	return ""
}
