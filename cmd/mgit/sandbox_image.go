package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/sandboxd/imageinstall"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// sandboxImageCmd is the host-operator surface for the digest-pinned,
// signed guest-image registry (images.lock + the Ed25519 trust root). It is
// host-local — it operates directly on the host config root, NOT through
// the daemon — because registering an image is a setup step done before the
// sandbox runs. Refs: FR-17.17, FR-17.29, FR-17.38, MGIT-11.10.6
func sandboxImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage signed, digest-pinned guest images (host operator)",
	}
	cmd.AddCommand(sandboxImageInitCmd(), sandboxImageAddCmd(), sandboxImageInstallCmd())
	return cmd
}

// sandboxImageInstallCmd fetches a shipped, pinned guest-image bundle for
// this host platform, verifies each artifact's sha256, ensures the trust
// root, and registers the image — the single step that makes the sandbox
// active without a manual kernel/rootfs build. Refs: MGIT-61.1
func sandboxImageInstallCmd() *cobra.Command {
	var from, name string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "install --from <dir-or-url> [--name base]",
		Short: "Fetch, verify, and register a shipped guest image (activates the sandbox)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return fmt.Errorf("--from is required: a directory or https URL holding manifest.json + the guest artifacts " +
					"(shipped image bundles are published with the release; see docs/INSTALL-SANDBOX.md)")
			}
			hostRoot, err := sandboxHostRoot()
			if err != nil {
				return err
			}
			in := &imageinstall.Installer{
				HostRoot: hostRoot,
				Audit:    printTrustRootAuditor{w: cmd.OutOrStdout()},
			}
			res, err := in.Install(cmd.Context(), from, name)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
					"image_ref": res.Ref, "platform": res.Platform,
				})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Installed %s for %s\nThe sandbox will use it automatically; or pass --image %s to mgit work.\n",
				res.Ref, res.Platform, res.Ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "directory or https URL with manifest.json + guest artifacts (required)")
	cmd.Flags().StringVar(&name, "name", "base", "image name to register")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// sandboxImageInitCmd generates the image-signing trust root (run once
// before `add`). Rerunning rotates the key. Refs: FR-17.38
func sandboxImageInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate (or rotate) the image-signing trust root",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hostRoot, err := sandboxHostRoot()
			if err != nil {
				return err
			}
			if _, err := images.GenerateTrustRoot(cmd.Context(), hostRoot,
				printTrustRootAuditor{w: cmd.OutOrStdout()}); err != nil {
				return fmt.Errorf("image init: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Image-signing trust root ready under %s\n", hostRoot)
			return nil
		},
	}
	return cmd
}

// sandboxImageAddCmd registers + signs a built guest image into images.lock
// and prints the digest-pinned reference the sandbox consumes.
func sandboxImageAddCmd() *cobra.Command {
	var name, kernel, rootfs, cmdline string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "add --name <n> --kernel <vmlinux> --rootfs <rootfs> [--cmdline <args>]",
		Short: "Register and sign a guest image into images.lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" || kernel == "" || rootfs == "" {
				return fmt.Errorf("--name, --kernel and --rootfs are required")
			}
			hostRoot, err := sandboxHostRoot()
			if err != nil {
				return err
			}
			// The trust-root signing key must exist (run `image init` first);
			// it stays host-side and never enters a guest (SEC-01).
			priv, err := images.LoadSigningKey(hostRoot)
			if err != nil {
				return fmt.Errorf("image add: %w (run `mgit sandbox image init` first)", err)
			}
			entry, err := images.BuildEntry(kernel, rootfs, cmdline)
			if err != nil {
				return fmt.Errorf("image add: %w", err)
			}
			ref, err := images.Register(hostRoot, name, entry, priv)
			if err != nil {
				return fmt.Errorf("image add: %w", err)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"image_ref": ref})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered %s\n", ref)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name (lowercase OCI-style; required)")
	cmd.Flags().StringVar(&kernel, "kernel", "", "path to the guest kernel (vmlinux; required)")
	cmd.Flags().StringVar(&rootfs, "rootfs", "", "path to the read-only rootfs image (required)")
	cmd.Flags().StringVar(&cmdline, "cmdline", "", "guest kernel command line")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// sandboxHostRoot returns the per-repo host config root (<repo>/.mgit/
// sandbox) that holds images.lock and the trust root — the same root the
// daemon is launched with. It requires the current directory to be an mgit
// repository. Refs: FR-17.13
func sandboxHostRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	if fi, statErr := os.Stat(filepath.Join(cwd, ".mgit")); statErr != nil || !fi.IsDir() {
		return "", fmt.Errorf("not an mgit repository (no .mgit in %s)", cwd)
	}
	return filepath.Join(cwd, ".mgit", "sandbox"), nil
}

// printTrustRootAuditor satisfies images.TrustRootAuditor by printing the
// trust-root change (carrying the key fingerprint) so the operator sees it.
type printTrustRootAuditor struct{ w io.Writer }

// RecordTrustRootChange writes the change detail.
func (a printTrustRootAuditor) RecordTrustRootChange(_ context.Context, detail string) error {
	_, _ = fmt.Fprintf(a.w, "trust root: %s\n", detail)
	return nil
}
