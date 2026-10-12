// Command xattrprobe is the GUEST workload for MGIT-225: it asks the question
// GNU `ls -l` asks of every entry it lists, on entries in the shared worktree.
//
// `ls -l` reads each entry's extended-attribute names WITHOUT following it
// (llistxattr). On a dangling symlink in the libkrun share that call answered
// ENOENT, because the share followed the link to its missing target, and GNU
// ls printed "ls: <link>: No such file or directory" on stderr while still
// listing the link correctly. The bases these tests boot carry no coreutils,
// so the question is asked directly, and the line ls would print is printed
// as an "LS-STDERR" line the host can count.
//
// A control asks the same question of a dangling link on a guest-local tmpfs,
// which follows no share: it must answer without ENOENT on every build, so a
// red result can never be a probe that cannot answer. Refs: MGIT-225
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

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

// probe asks the no-follow attribute question of one path and reports it.
func probe(label, path string) {
	if _, err := os.Lstat(path); err != nil {
		fmt.Printf("PROBE %s lstat=%v\n", label, err)
		return
	}
	if _, err := unix.Llistxattr(path, nil); err != nil {
		fmt.Printf("PROBE %s llistxattr=%v\n", label, err)
		if errors.Is(err, unix.ENOENT) {
			fmt.Printf("LS-STDERR ls: %s: No such file or directory\n", label)
		}
		return
	}
	fmt.Printf("PROBE %s llistxattr=ok\n", label)
}

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")
	defer fmt.Println("GUEST: done")

	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", 0, ""); err != nil {
		fmt.Printf("GUEST: mount tmpfs: %v\n", err)
		return
	}
	if err := os.Symlink("nothere", "/tmp/dangling"); err != nil {
		fmt.Printf("GUEST: control link: %v\n", err)
		return
	}
	probe("control/tmp/dangling", "/tmp/dangling")

	tokens := os.Getenv(envBootTokens)
	wtPath, wtFS, wtSrc := token(tokens, keyPath), token(tokens, keyFS), token(tokens, keySource)
	if wtPath == "" {
		fmt.Println("GUEST: no worktree descriptor")
		return
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil { //nolint:gosec // guest mount point
		fmt.Printf("GUEST: mkdir %s: %v\n", wtPath, err)
		return
	}
	if err := unix.Mount(wtSrc, wtPath, wtFS, 0, ""); err != nil {
		fmt.Printf("GUEST: mount %s at %s: %v\n", wtSrc, wtPath, err)
		return
	}
	src := filepath.Join(wtPath, "src")
	// A dangling link the GUEST makes in the share, as well as the one the
	// host put there: the ticket saw the line for both.
	if err := os.Symlink("nothere", filepath.Join(src, "guestdangling")); err != nil {
		fmt.Printf("GUEST: guest link: %v\n", err)
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		fmt.Printf("GUEST: read %s: %v\n", src, err)
		return
	}
	for _, e := range entries {
		probe("src/"+e.Name(), filepath.Join(src, e.Name()))
	}
}
