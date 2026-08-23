package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
)

// guestPair are the two LINUX binaries every guest base must carry: the
// supervisor that is PID 1 in the guest, and the CLI the agent runs there.
var guestPair = []string{"mgit", "mgit-guest"}

// guestBinSubdir is the directory name each install channel uses for the
// linux guest binaries. The archive layout and this lookup must agree on it;
// a packaging test pins both ends. Refs: MGIT-61.15
const guestBinSubdir = "guest"

// injectGuestBinaries installs mgit-guest and the mgit CLI, built for the
// guest's platform, into the base tree.
//
// They are injected by US rather than taken from the base, for two reasons.
// mgit-guest MUST be PID 1 — whatever entrypoint a base image declares is
// irrelevant to us, and an image shipping its own /sbin/mgit-guest must never
// end up mediating exec, land and the control plane. And the versions must
// match the host's, or the wire protocol can disagree across the boundary.
// Injection therefore runs AFTER extraction, overwriting whatever was there.
//
// Three sources, in order: the directory the operator named, the linux builds
// shipped beside this install, and finally a cross-build from source. The
// middle one is what makes `brew install mgit` sufficient — mgit-guest is
// guest-only and is never on a host PATH. Refs: MGIT-61.15, FR-17.11
func injectGuestBinaries(baseDir, guestBinDir, exePath string) error {
	if err := injectGuestBinariesWith(baseDir, resolveGuestBinaries(guestBinDir, exePath), buildGuestBinary); err != nil {
		return err
	}
	// Record the substrate that just froze those binaries into the base.
	//
	// It goes HERE, in the function that does the freezing, because that is
	// what the marker describes: the guest code inside a base is whatever this
	// build injected, and it never changes again. Recording it anywhere else
	// would be a second fact that could drift from the first — and both
	// compose paths reach this function, so neither can forget.
	//
	// Nothing compared composing-substrate to running-substrate before this,
	// because the composing substrate was written down nowhere; the door
	// everyone believed existed was a one-time cache miss during the in-tree
	// to content-addressed migration. Refs: MGIT-174, MGIT-147
	return guestbase.WriteComposedBy(baseDir, Version, "")
}

// guestBinarySource is where the linux guest binaries are coming from: a
// directory to copy them out of, or — when dir is empty — an mgit source
// checkout to cross-build from. `from` describes it in the terms a user
// would, and `exePath` is kept so a failure can report where we LOOKED.
type guestBinarySource struct {
	dir     string
	from    string
	exePath string
}

// resolveGuestBinaries applies the lookup order, which is fixed and is the
// whole point: what the operator named, then what this install shipped, then
// a source checkout.
//
// The source build is LAST because it is the only one that cannot work on a
// user's machine — it needs a Go toolchain and the mgit source, and a `brew
// install` has neither. It is also the fallback that hid two release blockers:
// tests run from inside the checkout cross-built the binaries on the spot, so
// nobody noticed the archive-relative lookup finding nothing.
// Refs: MGIT-65, MGIT-61.15
func resolveGuestBinaries(explicitDir, exePath string) guestBinarySource {
	if explicitDir != "" {
		return guestBinarySource{dir: explicitDir, from: "--guest-bin-dir " + explicitDir, exePath: exePath}
	}
	if dir := bundledGuestBinDir(exePath); dir != "" {
		return guestBinarySource{
			dir:     dir,
			from:    "the binaries shipped with this install (" + dir + ")",
			exePath: exePath,
		}
	}
	return guestBinarySource{from: "a source checkout (nothing else was available)", exePath: exePath}
}

// injectGuestBinariesWith installs the guest pair from a resolved source,
// cross-building only when the source names no directory. build is injected so
// a test can prove the fallback did NOT fire.
func injectGuestBinariesWith(baseDir string, src guestBinarySource, build func(pkg, out string) error) error {
	targets := []struct{ name, pkg, dest string }{
		{name: "mgit-guest", pkg: "./cmd/mgit-guest", dest: filepath.Join("sbin", "mgit-guest")},
		{name: "mgit", pkg: "./cmd/mgit", dest: filepath.Join("bin", "mgit")},
	}
	for _, tgt := range targets {
		out := filepath.Join(baseDir, tgt.dest)
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return fmt.Errorf("guest base: %w", err)
		}
		if src.dir != "" {
			if err := copyGuestBinary(filepath.Join(src.dir, tgt.name), out); err != nil {
				return err
			}
			continue
		}
		if err := build(tgt.pkg, out); err != nil {
			return fmt.Errorf("guest base: %w\n\n%s", err, missingGuestPairAdvice(src.exePath))
		}
	}
	return nil
}

// copyGuestBinary installs an operator-supplied guest binary into the tree,
// executable. It is the path that works without a Go toolchain or the mgit
// source — a host install carries neither.
func copyGuestBinary(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // operator-supplied path
	if err != nil {
		return fmt.Errorf("base set: read guest binary %s: %w", src, err)
	}
	//nolint:gosec // the guest supervisor and CLI must be executable
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("base set: install %s: %w", dst, err)
	}
	return nil
}

// guestBinChannel is one INSTALL CHANNEL's home for the guest pair, resolved
// for the binary that is running right now.
//
// The layout is channel-dependent, and stating it as one path was wrong: an
// earlier claim that "guest binaries ship in libexec/guest" described the
// INSTALLED tree and was reported as the archive's layout, conflating two
// channels. Verified against the real v0.6.1 artifact:
//
//	release archive        guest/mgit, guest/mgit-guest
//	install.sh / Homebrew  $PREFIX/libexec/guest/{mgit,mgit-guest}
//	go install             NEITHER — the gap that blocks composing at all
//
// Refs: MGIT-147, MGIT-65, MGIT-44, MGIT-61.15
type guestBinChannel struct {
	name string // the channel as a user would name it
	dir  string // where that channel would put the pair, for this binary
}

// guestBinChannels enumerates the channels in preference order: the archive's
// own layout first, because that is the one the running binary shipped with.
//
// Homebrew links binaries into <prefix>/bin and keeps non-PATH helpers in
// <prefix>/libexec — a guest/ inside bin/ would be linked onto PATH, which is
// precisely where mgit-guest must never be.
func guestBinChannels(exePath string) []guestBinChannel {
	if exePath == "" {
		return nil
	}
	// Resolve symlinks first. The ordinary way to put mgit on PATH is to
	// extract the archive and symlink the binary into /usr/local/bin, and
	// macOS reports the SYMLINK's path as the executable — so looking beside
	// it lands in /usr/local/bin, which ships no guest binaries. Refs: MGIT-65
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	binDir := filepath.Dir(exePath)
	return []guestBinChannel{
		{name: "release archive", dir: filepath.Join(binDir, guestBinSubdir)},
		{name: "install.sh / Homebrew", dir: filepath.Clean(filepath.Join(binDir, "..", "libexec", guestBinSubdir))},
	}
}

// bundledGuestBinDir returns the directory of the guest binaries shipped with
// this install, or "" when no channel has a COMPLETE pair.
//
// Completeness, not mere existence, is the test. A directory holding one of
// the two used to win the lookup and then fail deep inside injection with a
// bare "no such file or directory" naming a path the user never typed.
// Refs: MGIT-147, MGIT-65
func bundledGuestBinDir(exePath string) string {
	for _, ch := range guestBinChannels(exePath) {
		if missing := missingFromGuestBinDir(ch.dir); len(missing) == 0 {
			if resolved, err := filepath.EvalSymlinks(ch.dir); err == nil {
				return resolved
			}
			return ch.dir
		}
	}
	return ""
}

// missingFromGuestBinDir names which of the pair a directory lacks. A missing
// directory lacks both.
func missingFromGuestBinDir(dir string) []string {
	var missing []string
	for _, name := range guestPair {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// missingGuestPairAdvice explains a failed guest-pair lookup CHANNEL BY
// CHANNEL: what each one would have shipped, where we looked for it, and what
// was actually there.
//
// The failure mode being guarded is a `go install` user discovering at first
// sandbox launch that their install channel ships no guest binaries at all —
// a fact no single hardcoded path can express. Refs: MGIT-147
func missingGuestPairAdvice(exePath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The guest needs LINUX builds of mgit and mgit-guest (linux/%s). "+
		"Where they live depends on how mgit was installed:\n\n", guestArch())
	channels := guestBinChannels(exePath)
	for _, ch := range channels {
		fmt.Fprintf(&b, "  %-22s %s\n%-24s %s\n", ch.name, ch.dir, "", describeGuestBinDir(ch.dir))
	}
	if len(channels) == 0 {
		b.WriteString("  (this process cannot name its own binary, so no install layout could be checked)\n")
	}
	b.WriteString("  go install             ships neither: it builds the host binary only\n\n")
	b.WriteString("Install from a release archive or Homebrew, or supply the pair yourself:\n" +
		"  mgit sandbox base from <image> --guest-bin-dir <dir>   # holding `mgit` and `mgit-guest` built for linux/" +
		guestArch())
	return b.String()
}

// describeGuestBinDir says what is actually at a channel's directory, in the
// three states that matter: absent, incomplete, complete.
func describeGuestBinDir(dir string) string {
	if _, err := os.Stat(dir); err != nil {
		return "not found"
	}
	missing := missingFromGuestBinDir(dir)
	if len(missing) == 0 {
		return "found"
	}
	return "found, but missing " + strings.Join(missing, " and ")
}

// hostExecutablePath is os.Executable with the error folded into the empty
// string: a host that cannot name its own binary simply has no bundled guest
// binaries, which the caller already handles.
func hostExecutablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// buildGuestBinary cross-compiles one guest binary from an mgit source
// checkout — the developer convenience, used when no prebuilt directory was
// supplied. It fails when run outside the module, which is why the caller
// turns that into an actionable message.
func buildGuestBinary(pkg, out string) error {
	// No context: this is a one-shot developer-convenience build driven by an
	// interactive command, not a lifecycle the caller owns.
	//nolint:gosec,noctx // G204: fixed argv; out derives from the operator's own base dir
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=false",
		"-ldflags=-s -w -buildid=", "-o", out, pkg)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch(), "CGO_ENABLED=0")
	if combined, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s for the guest: %w: %s", pkg, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

// guestArch is the Linux architecture the guest runs, which matches the
// host's: libkrun uses hardware virtualization, so there is no emulation to
// cross architectures with. Refs: MGIT-61.15
func guestArch() string { return runtime.GOARCH }
