// Command artifactguest is the GUEST workload for the real-VM artifact-export
// e2e (MGIT-73): inside a real libkrun microVM it mounts the SEC-03 staged
// worktree at its identical path and BUILDS an artifact there — the way a real
// `npm install` or a build step would — so the host can then export it.
//
// It also plants the hostile cases in a separate subtree: a symlink to a host
// path outside the sandbox and a symlink out of the exported subtree. Those
// exist so the export refusals are proven against links a REAL guest really
// created through virtio-fs, not against links a test fabricated host-side.
// Refs: MGIT-73, SEC-03, ADR-011
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The host passes these in the guest environment, using the same boot-token
// contract every backend uses (guestboot); read directly here to keep this
// workload free of repo imports.
const (
	envBootTokens = "MGIT_GUEST_BOOT"
	keyPath       = "mgit.worktree"
	keyFS         = "mgit.worktree_fs"
	keySource     = "mgit.worktree_src"
)

// token extracts one space-separated key=value from the boot tokens.
func token(tokens, key string) string {
	for _, f := range strings.Fields(tokens) {
		if k, v, ok := strings.Cut(f, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// buildArtifact writes a small but realistic dependency tree: nested
// directories, an executable, and an in-tree relative symlink (the shape npm
// actually produces with its .bin links).
func buildArtifact(wt string) error {
	files := map[string]struct {
		content string
		mode    os.FileMode
	}{
		"node_modules/pkg/index.js":     {"module.exports = 1\n", 0o644},
		"node_modules/pkg/package.json": {"{\"name\":\"pkg\"}\n", 0o644},
		"node_modules/pkg/bin/run.sh":   {"#!/bin/sh\necho pkg\n", 0o755},
	}
	for rel, f := range files {
		path := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // guest-side artifact tree
			return err
		}
		if err := os.WriteFile(path, []byte(f.content), f.mode); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(wt, "node_modules", ".bin"), 0o755); err != nil { //nolint:gosec // guest-side artifact tree
		return err
	}
	return os.Symlink("../pkg/bin/run.sh", filepath.Join(wt, "node_modules", ".bin", "run"))
}

// plantEscapes creates the hostile subtree: a link to a host path outside the
// sandbox, and a link that leaves the exported subtree.
func plantEscapes(wt string) error {
	dir := filepath.Join(wt, "hostile")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // guest-side artifact tree
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ordinary.txt"), []byte("looks fine\n"), 0o644); err != nil { //nolint:gosec // guest-side artifact tree
		return err
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "abs-escape")); err != nil {
		return err
	}
	return os.Symlink("../node_modules/pkg/index.js", filepath.Join(dir, "out-of-subtree"))
}

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fmt.Printf("GUEST: mount /proc: %v\n", err)
	}

	tokens := os.Getenv(envBootTokens)
	wtPath, wtFS, wtSrc := token(tokens, keyPath), token(tokens, keyFS), token(tokens, keySource)
	if wtPath == "" || wtFS == "" || wtSrc == "" {
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (incomplete boot tokens %q)\n", tokens)
		fmt.Println("GUEST: done")
		return
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil { //nolint:gosec // guest mount point
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (mkdir: %v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	if err := unix.Mount(wtSrc, wtPath, wtFS, 0, ""); err != nil {
		fmt.Printf("GUEST-RESULT MOUNT = FAILED (mount %s %s at %s: %v)\n", wtSrc, wtFS, wtPath, err)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Printf("GUEST-RESULT MOUNT = OK (%s at %s)\n", wtSrc, wtPath)

	if err := buildArtifact(wtPath); err != nil {
		fmt.Printf("GUEST-RESULT BUILD = FAILED (%v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Println("GUEST-RESULT BUILD = OK")

	if err := plantEscapes(wtPath); err != nil {
		fmt.Printf("GUEST-RESULT ESCAPES = FAILED (%v)\n", err)
		fmt.Println("GUEST: done")
		return
	}
	fmt.Println("GUEST-RESULT ESCAPES = PLANTED")

	// The mode-fidelity measurement rides along with the same real boot
	// (MGIT-81): it needs a guest that has the share mounted, and booting a
	// second VM to look at permissions would measure a different VM.
	probeModes(wtPath)
	fmt.Println("GUEST: done")
}
