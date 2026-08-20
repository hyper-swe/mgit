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

// guestDirOf is the guest directory beside a binary, with symlinks resolved
// the way the lookup itself resolves them (macOS temp dirs are symlinked).
func guestDirOf(t *testing.T, exePath string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(exePath)
	require.NoError(t, err)
	return filepath.Join(filepath.Dir(resolved), guestBinSubdir)
}

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
		got, rerr := os.ReadFile(filepath.Join(cachedBaseDir(t, repo), p)) //nolint:gosec // test temp path
		require.NoError(t, rerr)
		assert.NotContains(t, string(got), "IMPOSTOR",
			"%s came from the image; the guest supervisor must always be ours", p)
	}
}

// THE SOURCE FALLBACK MUST NEVER BE WHAT MAKES THIS WORK.
//
// Both defects in MGIT-65 hid behind it. A first-run test that happened to run
// inside the mgit checkout cross-built the guest binaries on the spot, so the
// archive-relative lookup was never exercised and its absence went unnoticed
// until a machine with no Go toolchain tried it. The order is therefore
// asserted directly, and the fallback is asserted NOT to fire.
// Refs: MGIT-65, MGIT-61.15

func TestResolveGuestBinaries_PrefersTheOperatorThenTheInstallThenSource(t *testing.T) {
	explicit := fakeGuestBins(t)
	installed := installLayout(t)
	bare := filepath.Join(t.TempDir(), "mgit") // an install with no guest/ beside it

	tests := []struct {
		name     string
		explicit string
		exePath  string
		wantDir  string
		wantFrom string
	}{
		{
			name: "operator_supplied_wins", explicit: explicit, exePath: installed,
			wantDir: explicit, wantFrom: "--guest-bin-dir",
		},
		{
			name: "otherwise_the_install", explicit: "", exePath: installed,
			wantDir: guestDirOf(t, installed), wantFrom: "this install",
		},
		{
			name: "source_only_when_there_is_nothing_else", explicit: "", exePath: bare,
			wantDir: "", wantFrom: "a source checkout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveGuestBinaries(tt.explicit, tt.exePath)
			assert.Equal(t, tt.wantDir, got.dir)
			assert.Contains(t, got.from, tt.wantFrom)
		})
	}
}

func TestInjectGuestBinaries_NeverSourceBuildsWhenTheInstallShipsThem(t *testing.T) {
	// The regression this pins: a resolver that silently falls through to a
	// source build looks identical to one that works, right up until it meets
	// a machine with no Go toolchain.
	exePath := installLayout(t)
	refuseToBuild := func(pkg, _ string) error {
		t.Errorf("source-built %s while the install ships guest binaries", pkg)
		return nil
	}

	err := injectGuestBinariesWith(t.TempDir(), resolveGuestBinaries("", exePath), refuseToBuild)

	require.NoError(t, err)
}

func TestInjectGuestBinaries_SourceBuildIsStillReachedWhenNothingIsShipped(t *testing.T) {
	// The fallback must remain available for a developer working in a
	// checkout — it is last, not gone.
	built := map[string]bool{}
	src := resolveGuestBinaries("", filepath.Join(t.TempDir(), "mgit"))

	err := injectGuestBinariesWith(t.TempDir(), src, func(pkg, _ string) error {
		built[pkg] = true
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"./cmd/mgit-guest": true, "./cmd/mgit": true}, built)
}

// TestBundledGuestBinDir_FollowsASymlinkedInstall covers the ordinary way a
// user puts mgit on PATH: extract the archive somewhere, then symlink the
// binary into /usr/local/bin. macOS reports the SYMLINK's path as the
// executable, so looking for guest/ beside it lands in /usr/local/bin — which
// has none, and the source fallback then fails on a machine with no Go.
// Refs: MGIT-65
func TestBundledGuestBinDir_FollowsASymlinkedInstall(t *testing.T) {
	install := installLayout(t)
	onPath := filepath.Join(t.TempDir(), "mgit")
	require.NoError(t, os.Symlink(install, onPath))

	got := bundledGuestBinDir(onPath)

	assert.Equal(t, guestDirOf(t, install), got,
		"the guest binaries live beside the REAL binary, not beside the symlink")
}

// TestBundledGuestBinDir_FindsAHomebrewStyleLayout covers the install path
// most macOS users will actually take.
//
// Homebrew links binaries into <prefix>/bin and keeps non-PATH helper files
// in <prefix>/libexec — a `guest` directory inside bin/ would be linked onto
// PATH, which is exactly what mgit-guest must never be. So the lookup has to
// know both layouts: the archive's guest/ beside the binary, and libexec/guest
// one level up. Without this, `brew install` yields a working mgit that cannot
// compose a base, which is MGIT-65's second defect wearing a different hat.
// Refs: MGIT-65, MGIT-44
func TestBundledGuestBinDir_FindsAHomebrewStyleLayout(t *testing.T) {
	prefix := t.TempDir()
	exePath := filepath.Join(prefix, "bin", "mgit")
	require.NoError(t, os.MkdirAll(filepath.Dir(exePath), 0o750))
	require.NoError(t, os.WriteFile(exePath, []byte("host-mgit"), 0o600))

	guestDir := filepath.Join(prefix, "libexec", guestBinSubdir)
	require.NoError(t, os.MkdirAll(guestDir, 0o750))
	for _, n := range []string{"mgit", "mgit-guest"} {
		require.NoError(t, os.WriteFile(filepath.Join(guestDir, n), []byte("brewed-"+n), 0o600))
	}

	got := bundledGuestBinDir(exePath)

	resolved, err := filepath.EvalSymlinks(guestDir)
	require.NoError(t, err)
	assert.Equal(t, resolved, got, "a Homebrew-style install must be found")
}

// TestBundledGuestBinDir_PrefersTheAdjacentLayout keeps the archive's own
// layout authoritative when both channels have a complete pair — that is the
// one the running binary actually shipped with.
func TestBundledGuestBinDir_PrefersTheAdjacentLayout(t *testing.T) {
	prefix := t.TempDir()
	exePath := filepath.Join(prefix, "bin", "mgit")
	require.NoError(t, os.MkdirAll(filepath.Dir(exePath), 0o750))
	require.NoError(t, os.WriteFile(exePath, []byte("host-mgit"), 0o600))
	writeGuestPair(t, filepath.Join(prefix, "bin", guestBinSubdir), "archive")
	writeGuestPair(t, filepath.Join(prefix, "libexec", guestBinSubdir), "brewed")

	got := bundledGuestBinDir(exePath)

	resolved, err := filepath.EvalSymlinks(filepath.Join(prefix, "bin", guestBinSubdir))
	require.NoError(t, err)
	assert.Equal(t, resolved, got)
}

// writeGuestPair lays a complete guest pair into a channel's directory.
func writeGuestPair(t *testing.T, dir, marker string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o750))
	for _, n := range guestPair {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(marker+"-"+n), 0o600))
	}
}

// A HALF-POPULATED CHANNEL MUST NOT WIN THE LOOKUP.
//
// The guest-pair layout is channel-dependent, and a directory that exists but
// holds only one of the two used to be selected and then fail deep inside
// injection with a bare "no such file or directory" naming a path the user
// never typed. Refs: MGIT-147
func TestBundledGuestBinDir_SkipsAChannelMissingHalfThePair(t *testing.T) {
	prefix := t.TempDir()
	exePath := filepath.Join(prefix, "bin", "mgit")
	require.NoError(t, os.MkdirAll(filepath.Dir(exePath), 0o750))
	require.NoError(t, os.WriteFile(exePath, []byte("host-mgit"), 0o600))

	// The archive-style directory has only the host CLI, not the supervisor.
	adjacent := filepath.Join(prefix, "bin", guestBinSubdir)
	require.NoError(t, os.MkdirAll(adjacent, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(adjacent, "mgit"), []byte("half"), 0o600))
	// The Homebrew-style one is complete.
	writeGuestPair(t, filepath.Join(prefix, "libexec", guestBinSubdir), "brewed")

	got := bundledGuestBinDir(exePath)

	resolved, err := filepath.EvalSymlinks(filepath.Join(prefix, "libexec", guestBinSubdir))
	require.NoError(t, err)
	assert.Equal(t, resolved, got, "an incomplete channel must be skipped, not selected and then failed on")
}

// THE FAILURE MUST NAME THE CHANNELS, NOT ONE HARDCODED PATH.
//
// "Guest binaries ship in libexec/guest" is wrong as a general statement: the
// release archive puts them in guest/, install.sh and Homebrew in
// $PREFIX/libexec/guest, and `go install` ships NEITHER — which is the gap
// that blocks composing at all. A user hitting this must be told which
// channel was expected and what was actually found. Refs: MGIT-147
func TestInjectGuestBinaries_MissingPair_NamesEveryChannelAndWhatWasFound(t *testing.T) {
	t.Chdir(t.TempDir()) // outside the module, so the source build cannot work
	prefix := t.TempDir()
	exePath := filepath.Join(prefix, "bin", "mgit")
	require.NoError(t, os.MkdirAll(filepath.Dir(exePath), 0o750))
	require.NoError(t, os.WriteFile(exePath, []byte("host-mgit"), 0o600))
	// A half-populated Homebrew-style directory: present, but not usable.
	brewed := filepath.Join(prefix, "libexec", guestBinSubdir)
	require.NoError(t, os.MkdirAll(brewed, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(brewed, "mgit"), []byte("half"), 0o600))

	err := injectGuestBinaries(t.TempDir(), "", exePath)

	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "release archive", "the archive channel must be named")
	assert.Contains(t, msg, "install.sh / Homebrew", "the install.sh/Homebrew channel must be named")
	assert.Contains(t, msg, "go install", "the channel that ships neither must be named")
	assert.Contains(t, msg, filepath.Join(prefix, "bin", guestBinSubdir),
		"the archive-relative path we looked at must be shown")
	assert.Contains(t, msg, brewed, "the libexec path we looked at must be shown")
	assert.Contains(t, msg, "missing mgit-guest",
		"a directory that exists but lacks half the pair must say which half")
	assert.Contains(t, msg, "not found", "a channel with nothing there must say so")
}
