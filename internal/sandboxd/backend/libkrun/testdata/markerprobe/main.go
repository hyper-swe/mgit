// Command markerprobe is the GUEST workload for the concurrency isolation
// test: it mounts its own staged worktree and reports every marker it can
// read. The host asserts each sandbox sees ONLY its own — anything else is
// T6 cross-task contamination. Refs: SEC-03, MGIT-61.6
package main

import (
	"fmt"
	"os"
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

func main() {
	fmt.Println("GUEST: booted inside a real libkrun microVM")

	tokens := os.Getenv(envBootTokens)
	wtPath, wtFS, wtSrc := token(tokens, keyPath), token(tokens, keyFS), token(tokens, keySource)
	if wtPath == "" {
		fmt.Println("GUEST: no worktree descriptor")
		fmt.Println("GUEST: done")
		return
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil { //nolint:gosec // guest mount point
		fmt.Printf("GUEST: mkdir %s: %v\n", wtPath, err)
		fmt.Println("GUEST: done")
		return
	}
	if err := unix.Mount(wtSrc, wtPath, wtFS, 0, ""); err != nil {
		fmt.Printf("GUEST: mount %s at %s: %v\n", wtSrc, wtPath, err)
		fmt.Println("GUEST: done")
		return
	}

	b, err := os.ReadFile(wtPath + "/marker.txt") //nolint:gosec // guest worktree path
	if err != nil {
		fmt.Printf("GUEST: read marker: %v\n", err)
	} else {
		fmt.Printf("GUEST: FOUND %s\n", strings.TrimSpace(string(b)))
	}
	fmt.Println("GUEST: done")
}
