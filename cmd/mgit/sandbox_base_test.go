package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `mgit sandbox base set` is the bring-your-own-tree path (MGIT-61.15): point
// mgit at a Linux userspace directory and it becomes the read-only guest base
// every sandbox for this repo boots from. It is the GA-scoped half of that
// ticket; `base from <oci-ref>` is the fast follow.
//
// The base is a DIRECTORY because libkrun shares the guest root over
// virtio-fs and libkrunfw supplies the kernel — there is no rootfs image and
// no kernel to register. Refs: MGIT-61.15, ADR-010

// userspaceTree builds a plausible Linux userspace: the mount points
// mgit-guest needs at boot, plus a stand-in binary.
func userspaceTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"bin", "sbin", "proc", "dev", "tmp", "mnt", "usr/lib"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	//nolint:gosec // G306: a shell in a userspace tree must be executable
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin", "sh"), []byte("#!/bin/sh\n"), 0o700))
	return root
}

func TestSandboxBaseSet_PinsSignsAndInjectsTheSupervisor(t *testing.T) {
	repo := newRepo(t)
	tree := userspaceTree(t)

	out, err := initTrustRoot(t, repo)
	require.NoError(t, err, "trust root: %s", out)

	out, err = runBase(t, repo, "set", tree, "--guest-bin-dir", fakeGuestBins(t))
	require.NoError(t, err, "base set: %s", out)

	// A digest-pinned reference, the same shape image add prints.
	assert.Contains(t, out, "sha256:", "the base must be pinned by digest, got %q", out)

	// Requirement 4 of MGIT-61.15: mgit-guest is injected by US and runs as
	// PID 1 — a base image's own entrypoint must never displace it.
	guest := filepath.Join(tree, "sbin", "mgit-guest")
	info, err := os.Stat(guest)
	require.NoError(t, err, "mgit-guest must be injected into the base tree")
	assert.NotZero(t, info.Mode()&0o111, "the injected supervisor must be executable")

	// And the CLI, so an agent can run the checkpoint loop in the sandbox.
	_, err = os.Stat(filepath.Join(tree, "bin", "mgit"))
	require.NoError(t, err, "the mgit CLI must be injected into the base tree")
}

func TestSandboxBaseSet_RefusesATreeThatCannotBoot(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "not_a_directory",
			build:   func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
			wantErr: "base",
		},
		{
			// The failure a real BYO tree hits first: mgit-guest mounts into
			// /proc, /dev, /tmp and overlays using /mnt as scratch.
			name: "missing_mount_points",
			build: func(t *testing.T) string {
				root := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(root, "bin"), 0o750))
				return root
			},
			wantErr: "/proc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			if _, err := initTrustRoot(t, repo); err != nil {
				t.Fatalf("trust root: %v", err)
			}
			out, err := runBase(t, repo, "set", tt.build(t), "--guest-bin-dir", fakeGuestBins(t))
			require.Error(t, err, "an unbootable base must be refused, got %q", out)
			assert.Contains(t, err.Error()+out, tt.wantErr)
		})
	}
}

func TestSandboxBaseSet_IsIdempotentAndSurfacesAChangedTree(t *testing.T) {
	repo := newRepo(t)
	tree := userspaceTree(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	bins := fakeGuestBins(t)
	first, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	second, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	// Re-running against an unchanged tree must pin the same digest, or every
	// re-registration would look like a substitution.
	assert.Equal(t, digestOf(t, first), digestOf(t, second),
		"re-registering an unchanged base changed its digest")

	// A changed tree must produce a DIFFERENT digest — visible, never silent.
	require.NoError(t, os.WriteFile(filepath.Join(tree, "bin", "extra"), []byte("x"), 0o600))
	third, err := runBase(t, repo, "set", tree, "--guest-bin-dir", bins)
	require.NoError(t, err)
	assert.NotEqual(t, digestOf(t, first), digestOf(t, third),
		"a changed base kept its old digest; a swap would go unnoticed")
}

// digestOf extracts the sha256 digest from a printed reference.
func digestOf(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "sha256:")
	require.GreaterOrEqual(t, i, 0, "no digest in %q", out)
	return strings.TrimSpace(out[i:])
}

// fakeGuestBins stands in for a directory of prebuilt LINUX guest binaries —
// the path that works on a host install, which carries neither the mgit
// source nor a Go toolchain.
func fakeGuestBins(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"mgit", "mgit-guest"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("ELF-"+n), 0o600))
	}
	return dir
}

// initTrustRoot creates the repo's image-signing trust root, which a base
// must be signed into.
func initTrustRoot(t *testing.T, repo string) (string, error) {
	t.Helper()
	t.Chdir(repo)
	return runCLIOut(t, "sandbox", "image", "init")
}

// runBase executes `sandbox base <args...>` from inside repo.
func runBase(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(repo)
	return runCLIOut(t, append([]string{"sandbox", "base"}, args...)...)
}
