package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// guestBaseMountDirs are the mount points the in-guest supervisor needs at
// boot: it mounts /proc and /dev, a tmpfs on /tmp, and overlays a writable
// root using /mnt as the scratch for the upper/work/newroot layers.
//
// A tree missing any of them boots and then dies with a bare "no such file or
// directory" naming the mount, which says nothing about the tree being
// incomplete. The old ext4 image builder created them, so nothing had to
// check; a bring-your-own directory has no such guarantee.
// Refs: FR-17.3, FR-17.17, MGIT-61.15
var guestBaseMountDirs = []string{"proc", "dev", "tmp", "mnt"}

// sandboxBaseCmd manages the per-repo guest base: the Linux userspace tree
// every sandbox for this repository boots from, read-only, with each VM
// getting its own writable overlay.
//
// Under libkrun a base is just a DIRECTORY — libkrunfw supplies the kernel
// and the guest root is shared over virtio-fs — so there is no image to
// build, no kernel to pin, and composing one is a filesystem operation.
// Refs: MGIT-61.15, ADR-010
func sandboxBaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "base",
		Short: "Manage the per-repo guest base the sandbox boots from",
		Long: "The guest base is a Linux userspace tree shared read-only into every\n" +
			"sandbox for this repository. Point mgit at a tree you built or extracted,\n" +
			"and it is pinned by content digest and signed into images.lock so it\n" +
			"cannot change under a running task without being noticed.",
	}
	cmd.AddCommand(sandboxBaseSetCmd(), sandboxBaseFromCmd())
	return cmd
}

// sandboxBaseSetCmd validates a userspace tree, injects mgit + mgit-guest
// into it, and registers it as this repo's pinned, signed guest base.
func sandboxBaseSetCmd() *cobra.Command {
	var name, guestBinDir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "set <dir>",
		Short: "Use a directory as this repo's guest base (validates, injects mgit, pins and signs it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			baseDir, err := filepath.Abs(args[0])
			if err != nil {
				return fmt.Errorf("base set: %w", err)
			}
			hostRoot, err := sandboxHostRoot()
			if err != nil {
				return err
			}
			// The signing key stays host-side and never enters a guest (SEC-01).
			// First run has no trust root, and telling a user to go and make
			// one — after mgit told them to run THIS command — is guidance
			// that leads into a second wall. An existing key is reused, never
			// rotated. Refs: MGIT-65, FR-17.38
			priv, err := images.EnsureSigningKey(cmd.Context(), hostRoot,
				printTrustRootAuditor{w: cmd.OutOrStdout()})
			if err != nil {
				return fmt.Errorf("base %s: %w", "set", err)
			}
			if err := validateBaseTree(baseDir); err != nil {
				return err
			}
			// Inject BEFORE pinning: the digest must cover the binaries we
			// added, or the pin would describe a tree that never boots.
			if err := injectGuestBinaries(baseDir, guestBinDir, hostExecutablePath()); err != nil {
				return err
			}
			entry, err := images.BuildBaseEntry(baseDir)
			if err != nil {
				return fmt.Errorf("base set: %w", err)
			}
			ref, err := images.Register(hostRoot, name, entry, priv)
			if err != nil {
				return fmt.Errorf("base set: %w", err)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"image_ref": ref})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered guest base %s\n  from %s\n", ref, baseDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "base", "name to register the base under in images.lock")
	cmd.Flags().StringVar(&guestBinDir, "guest-bin-dir", "",
		"directory holding linux builds of mgit and mgit-guest to inject; "+
			"defaults to the ones shipped with this install")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// validateBaseTree rejects a tree that could not boot, naming what is missing
// and how to fix it. This runs before anything is injected or pinned, so a
// malformed base is a clear error rather than a guest that dies mid-boot.
// Refs: FR-17.3, MGIT-61.15
func validateBaseTree(baseDir string) error {
	info, err := os.Stat(baseDir)
	if err != nil {
		return fmt.Errorf("guest base %s: %w", baseDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf(
			"guest base %s is not a directory — libkrun shares the guest "+
				"root over virtio-fs, so a base is a directory tree, not an image", baseDir)
	}
	var missing []string
	for _, d := range guestBaseMountDirs {
		if fi, err := os.Stat(filepath.Join(baseDir, d)); err != nil || !fi.IsDir() {
			missing = append(missing, "/"+d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"guest base %s is missing the mount points the guest supervisor "+
				"needs at boot: %v. Create them first:\n  mkdir -p %s%v",
			baseDir, missing, baseDir, missing)
	}
	return nil
}

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
	return injectGuestBinariesWith(baseDir, resolveGuestBinaries(guestBinDir, exePath), buildGuestBinary)
}

// guestBinarySource is where the linux guest binaries are coming from: a
// directory to copy them out of, or — when dir is empty — a cross-build from
// an mgit source checkout. `from` describes it in the terms a user would.
type guestBinarySource struct {
	dir  string
	from string
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
// Refs: MGIT-65, MGIT-61.15, FR-17.11
func resolveGuestBinaries(explicitDir, exePath string) guestBinarySource {
	if explicitDir != "" {
		return guestBinarySource{dir: explicitDir, from: "--guest-bin-dir " + explicitDir}
	}
	if dir := bundledGuestBinDir(exePath); dir != "" {
		return guestBinarySource{dir: dir, from: "the binaries shipped with this install (" + dir + ")"}
	}
	return guestBinarySource{from: "a source checkout (nothing else was available)"}
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
			return fmt.Errorf(
				"guest base: %w\n\n"+
					"The guest needs LINUX builds of mgit and mgit-guest. This install "+
					"ships none beside it (expected a `guest` directory next to the mgit "+
					"binary), and building them here needs an mgit source checkout and a "+
					"Go toolchain. Either reinstall from a release archive, or supply "+
					"them with --guest-bin-dir <dir> containing `mgit` and `mgit-guest` "+
					"built for linux/%s", err, guestArch())
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

// bundledGuestBinDir returns the directory of the guest binaries shipped with
// this install, or "" when there are none.
//
// The release archive lays them out in a guest/ directory beside the host
// binary. The name carries no architecture because it needs none: libkrun uses
// hardware virtualization, so the guest architecture always equals the host's,
// and each archive ships only its own. Refs: MGIT-61.15, MGIT-44
func bundledGuestBinDir(exePath string) string {
	if exePath == "" {
		return ""
	}
	// Resolve symlinks first. The ordinary way to put mgit on PATH is to
	// extract the archive and symlink the binary into /usr/local/bin, and
	// macOS reports the SYMLINK's path as the executable — so looking beside
	// it lands in /usr/local/bin, which ships no guest binaries, and the
	// source fallback then fails on a machine with no Go. Refs: MGIT-65
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	binDir := filepath.Dir(exePath)
	// Two layouts, in preference order. The archive puts guest/ beside the
	// binary. Homebrew links binaries into <prefix>/bin and keeps non-PATH
	// helpers in <prefix>/libexec — a guest/ inside bin/ would be linked onto
	// PATH, which is precisely where mgit-guest must never be.
	for _, dir := range []string{
		filepath.Join(binDir, guestBinSubdir),
		filepath.Join(binDir, "..", "libexec", guestBinSubdir),
	} {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				return resolved
			}
			return filepath.Clean(dir)
		}
	}
	return ""
}

// guestBinSubdir is where a release install keeps the linux guest binaries,
// relative to the host binary. It is a name the archive layout and this
// lookup must agree on; a packaging test pins both ends. Refs: MGIT-61.15
const guestBinSubdir = "guest"

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

// sandboxBaseFromCmd composes this repo's guest base from an OCI image.
//
// The user pulls the image, so mgit redistributes nothing — no kernel, no
// busybox, no GPL corresponding-source obligation. What we contribute is our
// own Apache-2.0 binaries, injected on top. Refs: MGIT-61.15, ADR-010
func sandboxBaseFromCmd() *cobra.Command {
	var name, guestBinDir string
	var plainHTTP, asJSON bool
	cmd := &cobra.Command{
		Use:   "from <oci-ref>",
		Short: "Compose this repo's guest base from an OCI image (e.g. debian:12, node:22-slim)",
		Long: "Pulls a public OCI image, extracts its layers into this repo's guest base,\n" +
			"injects mgit and mgit-guest, then pins the composed tree by content digest\n" +
			"and signs it into images.lock.\n\n" +
			"The image supplies the Linux userspace your agent's toolchain needs; mgit\n" +
			"supplies the supervisor and the CLI. Because YOU pull the image, mgit\n" +
			"redistributes nothing.\n\n" +
			"Loading arbitrary tooling into the guest is safe because the guest is the\n" +
			"UNTRUSTED side — that is what the VM boundary is for, and a poisoned base\n" +
			"burns a throwaway microVM. The host store, egress policy, land airlock and\n" +
			"attestation signing are all enforced host-side and are unaffected by what\n" +
			"the base contains.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := guestbase.ParseRef(args[0])
			if err != nil {
				return err
			}
			hostRoot, err := sandboxHostRoot()
			if err != nil {
				return err
			}
			// First run has no trust root, and telling a user to go and make
			// one — after mgit told them to run THIS command — is guidance
			// that leads into a second wall. An existing key is reused, never
			// rotated. Refs: MGIT-65, FR-17.38
			priv, err := images.EnsureSigningKey(cmd.Context(), hostRoot,
				printTrustRootAuditor{w: cmd.OutOrStdout()})
			if err != nil {
				return fmt.Errorf("base %s: %w", "from", err)
			}

			baseDir := filepath.Join(hostRoot, "base")
			// Start from an empty tree: re-composing must not silently
			// inherit files from whatever was there before, or the pinned
			// digest would describe a mixture nobody can reproduce.
			if err := os.RemoveAll(baseDir); err != nil {
				return fmt.Errorf("base from: clear %s: %w", baseDir, err)
			}

			out := cmd.OutOrStdout()
			resolved, err := guestbase.Pull(cmd.Context(), ref, baseDir, guestbase.PullOptions{
				PlainHTTP: plainHTTP,
				Progress:  func(msg string) { _, _ = fmt.Fprintf(out, "  %s\n", msg) },
			})
			if err != nil {
				return err
			}

			// Images vary in which empty mount points they ship, and this
			// directory is one WE composed — so creating them is part of
			// composing, not a reason to reject an otherwise good image.
			// `base set` refuses instead, because there the tree is the
			// user's and mgit has no business writing into it.
			if err := ensureGuestMountDirs(baseDir); err != nil {
				return err
			}
			if err := assertLinuxUserspace(baseDir, resolved); err != nil {
				return err
			}
			if err := validateBaseTree(baseDir); err != nil {
				return err
			}
			if warning := checkLibcCoherence(baseDir); warning != "" {
				_, _ = fmt.Fprintf(out, "  warning: %s\n", warning)
			}
			if err := injectGuestBinaries(baseDir, guestBinDir, hostExecutablePath()); err != nil {
				return err
			}

			entry, err := images.BuildBaseEntry(baseDir)
			if err != nil {
				return fmt.Errorf("base from: %w", err)
			}
			// Provenance: record WHERE it came from alongside WHAT is pinned.
			// The tree digest is what boot verifies; the OCI reference is how
			// a human traces it back.
			entry.Source = resolved.String()
			pinnedRef, err := images.Register(hostRoot, name, images.Sign(name, entry, priv), priv)
			if err != nil {
				return fmt.Errorf("base from: %w", err)
			}
			if asJSON {
				return json.NewEncoder(out).Encode(map[string]string{
					"image_ref": pinnedRef, "source": resolved.String(),
				})
			}
			_, _ = fmt.Fprintf(out, "Registered guest base %s\n  from %s\n", pinnedRef, resolved)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "base", "name to register the base under in images.lock")
	cmd.Flags().StringVar(&guestBinDir, "guest-bin-dir", "",
		"directory holding linux builds of mgit and mgit-guest to inject; "+
			"defaults to the ones shipped with this install")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false,
		"talk to the registry over http (local mirrors and tests only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// ensureGuestMountDirs creates the empty mount points the in-guest supervisor
// needs. Refs: FR-17.3, MGIT-61.15
func ensureGuestMountDirs(baseDir string) error {
	for _, d := range guestBaseMountDirs {
		if err := os.MkdirAll(filepath.Join(baseDir, d), 0o755); err != nil { //nolint:gosec // guest mount points must be world-traversable, as in any rootfs
			return fmt.Errorf("guest base: create mount point /%s: %w", d, err)
		}
	}
	return nil
}

// guestShells are the places a usable base keeps its shell. Any one of them is
// enough; which one it is does not matter to us.
var guestShells = []string{"bin/sh", "usr/bin/sh", "bin/bash", "bin/busybox"}

// assertLinuxUserspace refuses a scratch or distroless image.
//
// Such an image is a single binary, not a userspace: mgit-guest would boot in
// it, and then every agent command would fail because there is no shell to run
// it with. Catching that here — while the user is still looking at the pull
// they just typed — is far kinder than an exec failure inside a task hours
// later. Refs: MGIT-61.15, FR-17.11
func assertLinuxUserspace(baseDir string, source fmt.Stringer) error {
	for _, sh := range guestShells {
		// Lstat, not Stat: bin/sh is usually a symlink, and on a tree we have
		// not chrooted into, its target does not resolve from here.
		if _, err := os.Lstat(filepath.Join(baseDir, sh)); err == nil {
			return nil
		}
	}
	return fmt.Errorf(
		"the image %s has no shell (looked for %s), so it is not a Linux userspace "+
			"a sandbox can run commands in.\n\n"+
			"Scratch and distroless images cannot be guest bases; distro images "+
			"(debian:12, ubuntu:24.04, alpine:3.20) and language images "+
			"(node:22, python:3.12, golang:1.23) all work",
		source, strings.Join(guestShells, ", "))
}

// globAny reports whether name matches anything in the tree's library
// directories, at either the top level or under a multiarch tuple.
func globAny(baseDir, name string) bool {
	for _, pattern := range []string{
		filepath.Join(baseDir, "lib*", name),
		filepath.Join(baseDir, "lib*", "*", name),
		filepath.Join(baseDir, "usr", "lib*", name),
		filepath.Join(baseDir, "usr", "lib*", "*", name),
	} {
		if hits, _ := filepath.Glob(pattern); len(hits) > 0 {
			return true
		}
	}
	return false
}

// checkLibcCoherence warns when a base's C library will not match the
// toolchains a user is likely to add to it.
//
// It WARNS rather than refuses: a musl base is perfectly valid, and plenty of
// workloads are static or musl-native. What is not valid is silence — an
// alpine base with a glibc-linked toolchain fails at runtime with "no such
// file or directory" naming the INTERPRETER, which is one of the most
// confusing errors in Linux. Refs: MGIT-61.15 req 6
func checkLibcCoherence(baseDir string) string {
	// Both libraries live one or two levels under a lib directory, the second
	// level being the multiarch tuple (lib/x86_64-linux-gnu/libc.so.6).
	musl := globAny(baseDir, "ld-musl-*")
	glibc := globAny(baseDir, "libc.so.6")
	switch {
	case musl && !glibc:
		return "this base uses musl (alpine-style). Toolchains built against " +
			"glibc will fail inside it with a confusing \"no such file or directory\" " +
			"naming the dynamic loader. Prefer musl-native or statically linked tools, " +
			"or use a glibc base such as debian-slim."
	case !musl && !glibc:
		return "no C library found in this base. Dynamically linked tools will not " +
			"run in it; only static binaries will."
	}
	return ""
}

// defaultGuestBaseName is the name `mgit sandbox base` registers under, and
// the one a launch boots when no --image was given.
const defaultGuestBaseName = "base"

// repoGuestBaseRef returns the digest-pinned reference of this repo's guest
// base, or an error naming the one command that creates one.
//
// A digest is not something a person chooses — it is the output of composing
// a base. Requiring --image on every launch would mean copying a 64-hex string
// between commands, where a typo is indistinguishable from tampering. So a
// registered base is used automatically, and its ABSENCE fails closed: mgit
// ships no default base (we redistribute no kernel and no userspace), and
// guessing one would silently boot something the user never chose.
// Refs: MGIT-61.15, FR-17.17
func repoGuestBaseRef() (string, error) {
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return "", err
	}
	ref, err := images.PinnedRef(hostRoot, defaultGuestBaseName)
	if errors.Is(err, images.ErrNoSuchImage) {
		return "", fmt.Errorf(
			"this repo has no guest base, so there is nothing to boot.\n\n" +
				"Compose one from any Linux image — mgit pulls it, and ships no " +
				"userspace of its own:\n" +
				"  mgit sandbox base from debian:12\n\n" +
				"Pick an image that already carries your task's toolchain " +
				"(node:22, python:3.12, golang:1.23), or pass an explicitly " +
				"pinned --image <name>@sha256:<hex>")
	}
	if err != nil {
		return "", fmt.Errorf("guest base: %w", err)
	}
	return ref, nil
}
