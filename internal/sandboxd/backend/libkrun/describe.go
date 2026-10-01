package libkrun

import (
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// describeLoaded builds the daemon's VMM report from the shared objects the
// dynamic loader has mapped into this process, read AFTER a libkrun context
// was created (which is what makes libkrun load libkrunfw), and from the
// networking probe's verdict.
//
// It reads the loader's own record instead of re-resolving names, because the
// question is not "could some libkrunfw be found somewhere" but "did the
// libkrun this daemon links find one": libkrun dlopen()s libkrunfw by leaf
// name from inside its own code, so only its search path counts.
// Refs: MGIT-229
func describeLoaded(loaded []string, netErr, bundleErr error) model.VMMReport {
	krun, krunfw := findLibrary(loaded, isLibkrun), findLibrary(loaded, isLibkrunfw)
	r := model.VMMReport{
		VMM: model.BackendLibkrun,
		Libraries: []model.VMMLibrary{
			{Name: "libkrun", Path: krun},
			{Name: "libkrunfw", Path: krunfw},
		},
	}
	if krun == "" {
		r.Problems = append(r.Problems, "libkrun is not loaded in this process, so this build cannot "+
			"have linked it; no guest can boot")
	}
	if krunfw == "" {
		where := "on the loader's search path"
		if krun != "" {
			where = "beside libkrun (" + filepath.Dir(krun) + ") or on the loader's search path"
		}
		r.Problems = append(r.Problems, "libkrun did not load libkrunfw, the library that carries the "+
			"guest kernel, so no guest can boot; libkrun looks for it "+where)
	}
	if netErr != nil {
		r.Problems = append(r.Problems, netErr.Error())
	}
	if bundleErr != nil {
		r.Problems = append(r.Problems, bundleErr.Error())
	}
	return r
}

// findLibrary returns the first loaded path whose file name matches.
func findLibrary(loaded []string, match func(base string) bool) string {
	for _, p := range loaded {
		if match(filepath.Base(p)) {
			return p
		}
	}
	return ""
}

// isLibkrun matches libkrun's names on Linux (libkrun.so, libkrun.so.1…) and
// macOS (libkrun.dylib, libkrun.1.dylib) — and never libkrunfw's, whose name
// starts with libkrun's.
func isLibkrun(base string) bool {
	return base == "libkrun.so" || strings.HasPrefix(base, "libkrun.so.") ||
		(strings.HasPrefix(base, "libkrun.") && strings.HasSuffix(base, ".dylib"))
}

// isLibkrunfw matches libkrunfw's names on Linux and macOS.
func isLibkrunfw(base string) bool {
	return base == "libkrunfw.so" || strings.HasPrefix(base, "libkrunfw.so.") ||
		(strings.HasPrefix(base, "libkrunfw.") && strings.HasSuffix(base, ".dylib"))
}
