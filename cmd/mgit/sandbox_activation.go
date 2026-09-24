package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"
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
	tail, marked := lastAttempt(readDaemonLogTail(logPath))
	if tail == "" {
		if !marked {
			return "" // an older CLI's log, or none: nothing to add
		}
		return "\nthe daemon exited before its first log line" + neverSpokeHint(logPath)
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
	if strings.HasPrefix(m[1], "@rpath/") {
		return m[1]
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
	if bundled, ok := strings.CutPrefix(lib, "@rpath/"); ok {
		// Carry a patched libkrun in the macOS build; fixes MGIT-225. Refs: MGIT-259
		return fmt.Sprintf(
			"%s is missing. It ships beside mgit-sandboxd, in lib/ of the release archive\n"+
				"(an installed mgit keeps it in <prefix>/lib/mgit), so no sandbox can start;\n"+
				"core mgit is unaffected. Reinstall mgit from the release archive and keep lib/\n"+
				"beside mgit-sandboxd.\n"+
				"Full prerequisites: docs/INSTALL-SANDBOX.md", path.Base(bundled))
	}
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

// daemonLogCap bounds daemon.log: past it the file is moved aside once, to
// <path>.1, before the next attempt appends, so attempts accumulate but the
// pair never holds more than two caps. Refs: MGIT-215
const daemonLogCap = 1 << 20

// daemonLogMarker prefixes the line the CLI writes before each spawn, so a
// reader — and daemonFailureDetail — can tell attempts apart. Refs: MGIT-215
const daemonLogMarker = "=== mgit spawns mgit-sandboxd"

// openDaemonLog opens the daemon's log for one more attempt: APPEND, never
// truncate. Five failed spawns in ten seconds once left one empty file, and
// the successful start then erased the record of what it had fixed
// (MGIT-214) — the old open truncated per attempt so the tail described
// "this spawn", which is exactly what deleted the five before it. Each
// attempt now begins with a start marker naming the CLI's version, its pid
// and the time; the file rotates once at daemonLogCap. Refs: MGIT-215
func openDaemonLog(path string, now time.Time) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > daemonLogCap {
		_ = os.Rename(path, path+".1") // best effort: a failed move costs one cap of boundedness, never a record
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // a path this process derived, owner-only dir
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "%s (mgit %s, pid %d) at %s ===\n", daemonLogMarker, Version, os.Getpid(), now.UTC().Format(time.RFC3339))
	return f, nil
}

// lastAttempt returns the log after its last start marker and whether a
// marker was present at all: a log written by an older CLI has none and is
// read whole, as before.
func lastAttempt(log string) (string, bool) {
	i := strings.LastIndex(log, daemonLogMarker)
	if i < 0 {
		return log, false
	}
	rest := log[i:]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[j+1:]
	} else {
		rest = ""
	}
	return strings.TrimSpace(rest), true
}

// neverSpokeHint says where the answer lives when the daemon wrote nothing:
// the attempts are in the log under their markers, and on darwin a process
// killed before its first instruction is a code-signature refusal the kernel
// recorded — eight times during MGIT-214 while daemon.log stayed empty.
// Refs: MGIT-215, MGIT-212, MGIT-214
func neverSpokeHint(logPath string) string {
	s := " (every attempt is appended to " + logPath + " under a start marker; this one wrote nothing)"
	if runtime.GOOS == "darwin" {
		s += ".\nOn macOS a daemon killed before it runs is a code-signature refusal — a quarantined download, " +
			"or a binary overwritten in place while a daemon ran from it — and the kernel recorded it: " +
			"`log show --predicate 'process == \"kernel\"' --last 10m | grep -E 'AMFI|ASP|code signature'`; " +
			"`mgit doctor` names the fixes (daemon/loads)"
	}
	return s
}
