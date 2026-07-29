package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A HOST INSTALL MUST BE ABLE TO COMPOSE A BASE.
//
// mgit-guest is guest-only: it is never on a host PATH, and cross-building it
// needs a Go toolchain and the mgit source, neither of which a `brew install`
// carries. So the release archive ships linux builds of mgit and mgit-guest
// next to the host binary, and composing a base finds them there.
// Refs: MGIT-61.15, FR-17.11

// installLayout fakes a release install: the host binary, with the guest
// binaries shipped beside it exactly as the archive lays them out.
func installLayout(t *testing.T) (exePath string) {
	t.Helper()
	dir := t.TempDir()
	exePath = filepath.Join(dir, "mgit")
	require.NoError(t, os.WriteFile(exePath, []byte("host-mgit"), 0o600))

	guestDir := filepath.Join(dir, guestBinSubdir)
	require.NoError(t, os.MkdirAll(guestDir, 0o750))
	for _, n := range []string{"mgit", "mgit-guest"} {
		require.NoError(t, os.WriteFile(filepath.Join(guestDir, n), []byte("shipped-"+n), 0o600))
	}
	return exePath
}

func TestInjectGuestBinaries_UsesTheBinariesShippedWithTheInstall(t *testing.T) {
	baseDir := t.TempDir()
	require.NoError(t, injectGuestBinaries(baseDir, "", installLayout(t)))

	for path, want := range map[string]string{
		filepath.Join("sbin", "mgit-guest"): "shipped-mgit-guest",
		filepath.Join("bin", "mgit"):        "shipped-mgit",
	} {
		got, err := os.ReadFile(filepath.Join(baseDir, path)) //nolint:gosec // test temp path
		require.NoError(t, err, "%s must be injected from the install", path)
		assert.Equal(t, want, string(got))
	}
}

func TestInjectGuestBinaries_ExplicitDirWinsOverTheShippedOnes(t *testing.T) {
	// --guest-bin-dir is how a developer tests a build they just made; it must
	// not be silently overridden by whatever the install shipped.
	baseDir := t.TempDir()
	require.NoError(t, injectGuestBinaries(baseDir, fakeGuestBins(t), installLayout(t)))

	got, err := os.ReadFile(filepath.Join(baseDir, "sbin", "mgit-guest")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "ELF-mgit-guest", string(got), "the explicitly supplied binary must win")
}

func TestInjectGuestBinaries_NoShippedBinariesAndNoSource_SaysWhatToDo(t *testing.T) {
	// The failure a user actually hits: a host install whose archive predates
	// the shipped guest binaries, run outside a source checkout. Both remedies
	// must be named, because only one of them applies to any given user.
	t.Chdir(t.TempDir()) // outside the module, so the source build cannot work
	err := injectGuestBinaries(t.TempDir(), "", filepath.Join(t.TempDir(), "mgit"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--guest-bin-dir")
	assert.Contains(t, err.Error(), "linux/"+guestArch(),
		"the message must name the architecture the guest needs")
}

// TestSandboxBaseFrom_InjectionOutranksTheImagesOwnFiles is a containment
// assertion, not a convenience one.
//
// mgit-guest is PID 1 in the guest. An image that ships its own /sbin/mgit-guest
// — by accident or by design — must never end up running as the supervisor
// that mediates exec, land and the vsock control plane. Injection happens
// after extraction for exactly this reason. Refs: MGIT-61.15, FR-17.11, SEC-03
func TestSandboxBaseFrom_InjectionOutranksTheImagesOwnFiles(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{
		"bin/sh":          "#!/bin/sh",
		"sbin/mgit-guest": "IMPOSTOR",
		"bin/mgit":        "IMPOSTOR",
	})
	defer srv.Close()

	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	out, err := runBase(t, repo, "from", ref,
		"--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.NoError(t, err, "base from: %s", out)

	for _, p := range []string{
		filepath.Join("sbin", "mgit-guest"),
		filepath.Join("bin", "mgit"),
	} {
		got, rerr := os.ReadFile(filepath.Join(repo, ".mgit", "sandbox", "base", p)) //nolint:gosec // test temp path
		require.NoError(t, rerr)
		assert.NotContains(t, string(got), "IMPOSTOR",
			"%s came from the image; the guest supervisor must always be ours", p)
	}
}
