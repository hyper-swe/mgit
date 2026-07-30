package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// daemonLogName is the per-repo capture of the spawned daemon's output, kept
// beside its socket in the runtime directory. The daemon is detached into its
// own session so nothing would otherwise read what it says on the way down.
// Refs: MGIT-61.15, NFR-17.6
const daemonLogName = "daemon.log"

// daemonLogTailBytes bounds how much of the capture is read back. The failure
// is always at the end, and an error message is not the place for a whole
// log file.
const daemonLogTailBytes = 1500

// missingLibraryRe matches both dynamic loaders' way of saying a library is
// not installed: macOS's `dyld: Library not loaded: /path/libfoo.1.dylib` and
// glibc's `error while loading shared libraries: libfoo.so.1`.
var missingLibraryRe = regexp.MustCompile(
	`(?:Library not loaded: |error while loading shared libraries: )([^\s':]+)`)

// daemonFailureDetail explains why a spawned daemon never came up, from the
// output it captured on the way down. It returns "" when it has nothing to
// add, so a caller can append it unconditionally.
//
// The most likely first-run failure on macOS is libkrun not being installed,
// and that one is invisible from inside the daemon: the dynamic loader
// refuses the binary before main() runs, so the capability check the daemon
// performs at startup (MGIT-61.14) never executes. The cause exists only in
// the child's output, which is why it is captured and read back rather than
// diagnosed in-process. Refs: MGIT-61.14, MGIT-61.15
func daemonFailureDetail(logPath string) string {
	tail := readTail(logPath, daemonLogTailBytes)
	if tail == "" {
		return ""
	}
	detail := "\nthe daemon reported:\n  " + strings.ReplaceAll(tail, "\n", "\n  ")
	if lib := missingLibrary(tail); lib != "" {
		detail += "\n\n" + missingLibraryRemedy(lib)
	}
	return detail
}

// missingLibrary returns the base name of the shared library the loader could
// not find, or "" when the failure was something else.
func missingLibrary(log string) string {
	m := missingLibraryRe.FindStringSubmatch(log)
	if m == nil {
		return ""
	}
	path := m[1]
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	return path
}

// missingLibraryRemedy names the one command that fixes a missing library.
//
// It is phrased around what the user has to DO. "Library not loaded" is
// accurate and useless: nothing in it says the sandbox needs a VMM, that the
// VMM is a separate package, or that one command installs it.
func missingLibraryRemedy(lib string) string {
	// libkrunfw ships as a dependency of the libkrun formula, so the same one
	// command covers either name.
	pkg := strings.TrimSuffix(strings.SplitN(lib, ".", 2)[0], "fw")
	if pkg != "libkrun" {
		return fmt.Sprintf(
			"%s is missing. mgit-sandboxd links it, so the sandbox cannot start; core\n"+
				"mgit is unaffected. Prerequisites: docs/INSTALL-SANDBOX.md", lib)
	}
	return fmt.Sprintf(
		"%s is missing. mgit-sandboxd links libkrun (the microVM hypervisor that runs\n"+
			"your sandboxes) — core mgit works without it, but no sandbox can start.\n"+
			"Install it:\n"+
			"  brew tap libkrun/krun && brew install libkrun\n"+
			"Full prerequisites: docs/INSTALL-SANDBOX.md", lib)
}

// readTail returns the last max bytes of a file, trimmed, or "" when the file
// is absent, unreadable or blank — all of which mean "nothing to report".
func readTail(path string, max int) string {
	data, err := os.ReadFile(path) //nolint:gosec // a path this process derived and wrote
	if err != nil {
		return ""
	}
	if len(data) > max {
		data = data[len(data)-max:]
		// Drop the partial first line the cut created.
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return strings.TrimSpace(string(data))
}
