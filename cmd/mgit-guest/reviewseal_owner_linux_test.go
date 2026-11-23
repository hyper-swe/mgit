//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These run the real ownership checks on files whose owner is known, instead of
// substituting their answers: "/" is root's with group 0, a file this test just
// made is the test user's.
func TestIsRootOwned_RealOwnership(t *testing.T) {
	if !isRootOwned("/") {
		t.Fatal("/ is not reported root-owned")
	}
	if isRootOwned(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing path was reported root-owned")
	}
	if os.Getuid() == 0 {
		t.Skip("the not-root half needs an unprivileged user")
	}
	own := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if isRootOwned(own) {
		t.Fatal("a file this user made was reported root-owned")
	}
}

func TestOwnedByRootWithGroup_RealOwnership(t *testing.T) {
	if err := ownedByRootWithGroup("/", 0); err != nil {
		t.Fatalf("/ is root:0: %v", err)
	}
	if err := ownedByRootWithGroup("/", 4242); err == nil {
		t.Fatal("/ was accepted as group 4242")
	}
	if err := ownedByRootWithGroup(filepath.Join(t.TempDir(), "missing"), 0); err == nil {
		t.Fatal("a missing path was accepted")
	}
	if os.Getuid() == 0 {
		t.Skip("the not-root half needs an unprivileged user")
	}
	own := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ownedByRootWithGroup(own, os.Getgid()); err == nil {
		t.Fatal("a file the reviewer's own user owns was accepted as sealed")
	}
}
