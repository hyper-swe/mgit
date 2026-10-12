package libkrun

// Carry a patched libkrun in the macOS build; fixes MGIT-225.
// Refs: MGIT-259

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
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
	if c.want != "1" {
		return nil
	}
	exe, err := c.exe()
	if err != nil {
		return fmt.Errorf("%w: resolve this daemon's executable to find the libkrun beside it: %w",
			model.ErrSandboxBackendUnavailable, err)
	}
	return requireBundledLibkrun(c.loaded(), exe, bundledLibkrunName())
}

// bundledLibkrunName is the file name of the libkrun a bundle carries.
func bundledLibkrunName() string {
	if runtime.GOOS == "darwin" {
		return "libkrun.1.dylib"
	}
	return "libkrun.so.1"
}

// requireBundledLibkrun returns nil when the libkrun in loaded is the same
// file as <daemon dir>/lib/<name> or <daemon dir>/../lib/mgit/<name>.
// Refs: MGIT-259
func requireBundledLibkrun(loaded []string, exe, name string) error {
	candidates := bundleCandidates(exe, name)
	krun := findLibrary(loaded, isLibkrun)
	if krun == "" {
		return fmt.Errorf("%w: no libkrun is loaded in this process; this daemon's libkrun ships beside it (%s)",
			model.ErrSandboxBackendUnavailable, strings.Join(candidates, " or "))
	}
	if got, err := os.Stat(krun); err == nil {
		for _, c := range candidates {
			if want, err := os.Stat(c); err == nil && os.SameFile(got, want) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: libkrun resolved to %s, not the copy shipped beside this daemon (%s); "+
		"reinstall mgit from the release archive and keep lib/ beside mgit-sandboxd "+
		"(install.sh and Homebrew put it in <prefix>/lib/mgit)",
		model.ErrSandboxBackendUnavailable, krun, strings.Join(candidates, " or "))
}

// bundleCandidates lists where a bundle's libkrun can sit, for the daemon's
// path as given and as resolved through any symlink.
func bundleCandidates(exe, name string) []string {
	paths := []string{exe}
	if real, err := filepath.EvalSymlinks(exe); err == nil && real != exe {
		paths = append(paths, real)
	}
	out := make([]string, 0, 2*len(paths))
	for _, p := range paths {
		dir := filepath.Dir(p)
		out = append(out, filepath.Join(dir, "lib", name), filepath.Join(dir, "..", "lib", "mgit", name))
	}
	return out
}
