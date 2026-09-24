package libkrun

// Carry a patched libkrun in the macOS build; fixes MGIT-225.
// Refs: MGIT-259

import (
	"os"
)

// bundled is "1" in a daemon built with its libkrun beside it; set at link
// time by scripts/release/build-darwin-sandboxd.sh.
var bundled string

// bundleCheck is the bundled-libkrun requirement with its inputs injected.
type bundleCheck struct {
	want   string
	loaded func() []string
	exe    func() (string, error)
}

// realBundleCheck is the requirement this build was linked with.
func realBundleCheck() bundleCheck {
	return bundleCheck{want: bundled, loaded: loadedLibraries, exe: os.Executable}
}

// err is nil for a build that is not bundled, and otherwise the verdict of
// requireBundledLibkrun on the libraries this process has loaded.
func (c bundleCheck) err() error {
	// Stub: the requirement is not enforced yet. The tests in bundle_test.go
	// name the contract and fail against this; the following commit replaces
	// this body with the check.
	return nil
}

// requireBundledLibkrun returns nil when the libkrun in loaded is the same
// file as <daemon dir>/lib/<name> or <daemon dir>/../lib/mgit/<name>.
// Refs: MGIT-259
func requireBundledLibkrun(loaded []string, exe, name string) error {
	// Stub: not enforced yet; bundle_test.go pins the contract.
	return nil
}
