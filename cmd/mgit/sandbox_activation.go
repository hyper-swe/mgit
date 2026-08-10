package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// daemonLogName is the per-repo capture of the spawned daemon's output, kept
// beside its socket in the runtime directory. The daemon is detached into its
// own session so nothing would otherwise read what it says on the way down.
//
// It is truncated on every spawn, so it holds one daemon's lifetime at most.
// That is deliberately not rotation: the file exists to explain a failure that
// happens seconds after a spawn, and a daemon that has been up long enough to
// write a large log is one that did not fail to start.
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
	tail := readDaemonLogTail(logPath)
	if tail == "" {
		return ""
	}
	detail := "\nthe daemon reported:\n  " + strings.ReplaceAll(humanizeDaemonLog(tail), "\n", "\n  ")
	if lib := missingLibrary(tail); lib != "" {
		detail += "\n\n" + missingLibraryRemedy(lib)
	}
	return detail
}

// humanizeDaemonLog renders the daemon's structured records as sentences.
//
// The daemon logs JSON (slog), and handing a user a raw slog record to read is
// barely better than not telling them anything. Lines that are not JSON — the
// dynamic loader's output, a panic — are the ones we most need to show, so
// they pass through untouched.
func humanizeDaemonLog(tail string) string {
	var out []string
	for _, line := range strings.Split(tail, "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Msg == "" {
			out = append(out, line)
			continue
		}
		sentence := rec.Msg
		if rec.Error != "" {
			sentence += ": " + rec.Error
		}
		out = append(out, sentence)
	}
	return strings.Join(out, "\n")
}

// missingLibrary returns the base name of the shared library the loader could
// not find, or "" when the failure was something else. path.Base, not
// filepath: both loaders emit slash-separated paths whatever the host.
func missingLibrary(log string) string {
	m := missingLibraryRe.FindStringSubmatch(log)
	if m == nil {
		return ""
	}
	return path.Base(m[1])
}

// missingLibraryRemedy names the commands that fix a missing library.
//
// It is phrased around what the user has to DO. "Library not loaded" is
// accurate and useless: nothing in it says the sandbox needs a VMM, that the
// VMM is a separate package, or how to install it.
//
// The libkrun install hint lives here rather than beside the backend that
// links it because the backend package is CGO- and build-tag-gated: on a host
// where this diagnosis matters, that package is exactly what failed to load.
//
// This hint is now the ONLY thing standing between a macOS user and a
// sandbox: the brew formula deliberately no longer declares libkrun as a
// dependency, because a third-party-tap dependency aborts the whole install
// for everyone who does not already have it (MGIT-75). That makes getting
// these commands right load-bearing, and the obvious ones do not work:
//
//   - `brew install libkrun` fails. Homebrew refuses to load a formula from
//     an untrusted third-party tap, and the bare name `libkrun` does not
//     match the formula's full name for its explicit-request escape hatch.
//   - `brew install libkrun/krun/libkrun` fails too, one step later, on
//     libkrunfw — libkrun's own dependency from the same tap. A transitive
//     dependency cannot be named on the command line at all.
//
// Whole-tap `brew trust` is the only step that clears both, so it comes
// first. All of this was established on a Homebrew prefix where libkrun was
// genuinely absent; on a machine that already has it, none of these commands
// has to load anything and they all appear to work.
// Refs: MGIT-75, MGIT-61.15
func missingLibraryRemedy(lib string) string {
	detail := fmt.Sprintf(
		"%s is missing. mgit-sandboxd links it, so no sandbox can start; core mgit\n"+
			"is unaffected.\n", lib)
	// libkrunfw ships as a dependency of the libkrun formula, so one sequence
	// covers either name.
	if strings.HasPrefix(lib, "libkrun") {
		detail += "Install the microVM hypervisor that runs your sandboxes (mgit does not\n" +
			"install it for you — it is a third-party tap you have to trust first):\n" +
			"  brew tap libkrun/krun\n" +
			"  brew trust libkrun/krun\n" +
			"  brew install libkrun\n"
	}
	return detail + "Full prerequisites: docs/INSTALL-SANDBOX.md"
}

// readDaemonLogTail returns the last daemonLogTailBytes of the capture,
// trimmed, or "" when the file is absent, unreadable or blank — all of which
// mean "nothing to report".
func readDaemonLogTail(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // a path this process derived and wrote
	if err != nil {
		return ""
	}
	if len(data) > daemonLogTailBytes {
		data = data[len(data)-daemonLogTailBytes:]
		// Drop the partial first line the cut created.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return strings.TrimSpace(string(data))
}
