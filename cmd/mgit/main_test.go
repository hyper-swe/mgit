package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// TestMain makes this package's tests hermetic with respect to the machine
// they run on, and it is a correctness guard rather than tidiness.
//
// `go test` starts each test in its PACKAGE directory — which, for this
// package, is inside the mgit checkout itself. Sandbox commands resolve a
// repo's host root by walking UPWARDS from the working directory (MGIT-57), so
// without this a test that touches the sandbox would find the DEVELOPER'S OWN
// repository and operate on it: read its images.lock, and — since MGIT-147 —
// migrate its guest base out of the tree. That happened once during this
// ticket's own work, which is why the guard is here.
//
// Two things are therefore pinned before any test runs: a working directory
// with no mgit repository above it, and a scratch guest-base cache, so nothing
// can write to the real one either. Tests that need a repository chdir into
// one of their own. Refs: MGIT-147, MGIT-57
func TestMain(m *testing.M) {
	scratch, err := os.MkdirTemp("", "mgit-cli-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermetic test setup: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chdir(scratch); err != nil {
		fmt.Fprintf(os.Stderr, "hermetic test setup: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv(basecache.EnvRoot, scratch+"/base-cache"); err != nil {
		fmt.Fprintf(os.Stderr, "hermetic test setup: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(scratch)
	os.Exit(code)
}
