package doctor

import (
	"context"
	"fmt"
	"strings"
)

// DaemonLoad is what running this host's sandbox daemon binary with
// --version found. Version is set when it loaded and answered; otherwise
// Output holds what it printed on the way down, MissingLibrary the library
// the dynamic loader named (if that is what killed it), and Remedy the
// platform's install commands for that library when the probe knows them.
// Refs: MGIT-206
type DaemonLoad struct {
	Path           string
	Version        string
	Output         string
	MissingLibrary string
	Remedy         string
}

// DaemonLoadsCheck reports whether the sandbox daemon binary on this host
// can load at all.
//
// From MGIT-206: a libkrun dependency vanished from a developer's Mac
// mid-day, every `mgit run` died at daemon activation with the loader's
// "Library not loaded" line — and `mgit doctor` said "No check found a
// known-bad condition", because every daemon row read the RECORDS of
// daemons already running and nothing spawned the binary. A host on which
// no sandbox can start is exactly what doctor exists to name.
// Refs: MGIT-206, MGIT-61.15, R-H300 rule 5
type DaemonLoadsCheck struct {
	// Probe locates the daemon binary and runs it with --version. An error
	// means it could not be run at all (no binary on a core-only install).
	Probe func(ctx context.Context) (DaemonLoad, error)
}

// Name implements Check.
func (DaemonLoadsCheck) Name() string { return "daemon/loads" }

// Run implements Check.
func (c DaemonLoadsCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-206"}
	load, err := c.Probe(ctx)
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not run this host's sandbox daemon binary"
		return r
	}
	if load.Version != "" {
		r.Status = StatusOK
		r.Summary = fmt.Sprintf("%s loads and reports %s", load.Path, load.Version)
		return r
	}
	r.Status = StatusFailed
	said := firstLine(load.Output)
	if load.MissingLibrary != "" {
		r.Summary = fmt.Sprintf("%s cannot start: %s is missing — no sandbox can start on this host, "+
			"whatever the sandbox verbs say (the loader said: %s)", load.Path, load.MissingLibrary, said)
	} else {
		r.Summary = fmt.Sprintf("%s cannot start — no sandbox can start on this host (it said: %s)", load.Path, said)
	}
	r.Remedy = load.Remedy
	if r.Remedy == "" {
		r.Remedy = "Run the daemon by hand (`" + load.Path + " --version`) and read its words; " +
			"prerequisites and recovery: docs/INSTALL-SANDBOX.md"
	}
	return r
}

// firstLine is the first non-empty line of a capture, or a stated blank.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return "(nothing)"
}
