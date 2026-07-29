package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

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
	cmd.AddCommand(sandboxBaseSetCmd())
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
			priv, err := images.LoadSigningKey(hostRoot)
			if err != nil {
				return fmt.Errorf("base set: %w (run `mgit sandbox image init` first)", err)
			}
			if err := validateBaseTree(baseDir); err != nil {
				return err
			}
			// Inject BEFORE pinning: the digest must cover the binaries we
			// added, or the pin would describe a tree that never boots.
			if err := injectGuestBinaries(baseDir, guestBinDir); err != nil {
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
		"directory holding linux guest builds of mgit and mgit-guest; "+
			"defaults to building them from an mgit source checkout")
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
		return fmt.Errorf("base set: guest base %s: %w", baseDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf(
			"base set: guest base %s is not a directory — libkrun shares the guest "+
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
			"base set: guest base %s is missing the mount points the guest supervisor "+
				"needs at boot: %v. Create them first:\n  mkdir -p %s%v",
			baseDir, missing, baseDir, missing)
	}
	return nil
}

// injectGuestBinaries builds mgit-guest and the mgit CLI for the guest's
// platform and installs them into the base tree.
//
// They are injected by US rather than expected from the base, for two
// reasons. mgit-guest MUST be PID 1 — whatever entrypoint a base image
// declares is irrelevant to us and must never displace it — and the versions
// must match the host's, or the exec/land wire protocol can disagree across
// the boundary. Refs: MGIT-61.15, FR-17.11
func injectGuestBinaries(baseDir, guestBinDir string) error {
	targets := []struct{ name, pkg, dest string }{
		{name: "mgit-guest", pkg: "./cmd/mgit-guest", dest: filepath.Join("sbin", "mgit-guest")},
		{name: "mgit", pkg: "./cmd/mgit", dest: filepath.Join("bin", "mgit")},
	}
	for _, tgt := range targets {
		out := filepath.Join(baseDir, tgt.dest)
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return fmt.Errorf("base set: %w", err)
		}
		if guestBinDir != "" {
			if err := copyGuestBinary(filepath.Join(guestBinDir, tgt.name), out); err != nil {
				return err
			}
			continue
		}
		if err := buildGuestBinary(tgt.pkg, out); err != nil {
			return fmt.Errorf(
				"base set: %w\n\n"+
					"The guest needs LINUX builds of mgit and mgit-guest, which a host "+
					"install does not carry (mgit-guest is guest-only and is not shipped "+
					"on PATH). Either run this from an mgit source checkout, or supply "+
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
