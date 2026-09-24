package doctor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// ServingDaemonVersionCheck compares the version the daemon SERVING THIS
// REPOSITORY recorded when it started with this CLI's own version.
//
// From MGIT-221: an upgrade replaces the binaries on disk and leaves a
// running daemon running. A 0.6.7 daemon went on answering a 0.6.8 CLI, with
// no refusal and no warning, while every daemon row read ok, because
// daemon/loads runs the binary ON DISK and the daemons/* rows judge
// lifetime and duplicates, never which build answers. The new release's
// daemon-side and guest-side fixes were absent while doctor vouched for
// them. The layer that owns "which build answers" is the running daemon's
// own record, written by that daemon at start; this row reads it.
//
// Version and commit are compared; the build stamp is not, because binaries
// built in separate jobs of one release can differ there. A mismatch is a
// DIFFERENCE, stated with both sides, the pid and the remedy. No daemon
// serving this repository is not-checked, with that said: there is nothing
// running to vouch for, and an ok about a daemon that is not there would be
// the silence R-H300 forbids. Refs: MGIT-221, MGIT-174, R-H300 rule 2
type ServingDaemonVersionCheck struct {
	// List reads and judges every daemon record for this user on this host.
	List func(ctx context.Context) ([]daemonrec.Listed, error)
	// RepoRoot is the repository whose daemon this directory talks to.
	RepoRoot func() (string, error)
	// CLI is this CLI's own version line, as `mgit --version` prints it.
	CLI string
}

// Name implements Check.
func (ServingDaemonVersionCheck) Name() string { return "daemon/serving-version" }

// buildLine reads "<version> (commit: <commit>, ..." as both binaries print it.
var buildLine = regexp.MustCompile(`^(\S+) \(commit: ([^,)]+)`)

// unstamped is the commit a build carries with neither -ldflags nor
// version-control information: two such builds look identical whatever
// they were built from.
const unstamped = "none"

// buildID is what identifies a build: its version and its commit.
type buildID struct{ version, commit string }

// parseBuild reads a version line; a line without that shape is its own
// version with no commit.
func parseBuild(line string) buildID {
	line = strings.TrimSpace(line)
	if m := buildLine.FindStringSubmatch(line); m != nil {
		return buildID{m[1], m[2]}
	}
	return buildID{version: line}
}

func (b buildID) String() string {
	if b.commit == "" {
		return b.version
	}
	return fmt.Sprintf("%s (commit: %s)", b.version, b.commit)
}

// verdict judges one serving daemon's build against the CLI's.
func verdict(daemon, cli buildID) Status {
	switch {
	case daemon.version == "" || daemon.version != cli.version:
		return StatusDiffers
	case daemon.commit == unstamped || cli.commit == unstamped:
		return StatusNotChecked
	case daemon.commit != cli.commit:
		return StatusDiffers
	}
	return StatusOK
}

// Run implements Check.
func (c ServingDaemonVersionCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-221"}
	root, err := c.RepoRoot()
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not tell which repository's daemon this directory talks to"
		return r
	}
	listed, err := c.List(ctx)
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not read this host's sandbox daemon records"
		return r
	}
	want := resolvedRoot(root)
	var serving []daemonrec.Listed
	for _, d := range listed {
		if d.Status.Alive && resolvedRoot(d.Record.RepoRoot) == want {
			serving = append(serving, d)
		}
	}
	cli := parseBuild(c.CLI)
	if len(serving) == 0 {
		r.Status = StatusNotChecked
		r.Reason = "no daemon serves this repository right now"
		r.Summary = fmt.Sprintf("no daemon serves this repository right now, so no running build to compare; "+
			"the next sandbox command starts one from this install (%s)", cli)
		return r
	}
	for _, d := range serving {
		got := parseBuild(d.Record.Version)
		switch verdict(got, cli) {
		case StatusDiffers:
			return c.differs(r, d, root, got.String(), cli.String())
		case StatusNotChecked:
			r.Status = StatusNotChecked
			r.Reason = "a build with no build stamp (commit: none) cannot be told apart from another"
			r.Summary = fmt.Sprintf("cannot tell whether the daemon serving this repository (pid %d, %s) is this "+
				"CLI's build (%s): one of them carries no build stamp — neither -ldflags nor version-control "+
				"information — so two such builds look the same whatever they were built from",
				d.Record.PID, got, cli)
			return r
		}
	}
	d := serving[0]
	r.Status = StatusOK
	r.Summary = fmt.Sprintf("the daemon serving this repository (pid %d) runs %s, the same build as this CLI",
		d.Record.PID, parseBuild(d.Record.Version))
	return r
}

// differs states a daemon that answers with another build than this CLI's.
func (c ServingDaemonVersionCheck) differs(r Result, d daemonrec.Listed, root, got, cli string) Result {
	runs := "runs " + got
	if got == "" {
		runs = "recorded no version (it predates version records)"
	}
	r.Status = StatusDiffers
	r.Summary = fmt.Sprintf("the daemon serving this repository (pid %d, started %s) %s; this CLI is %s — "+
		"it answers every sandbox command, so the daemon-side and guest-side changes of this CLI's build are "+
		"absent until it restarts", d.Record.PID, d.Record.StartedAt.UTC().Format(time.RFC3339), runs, cli)
	r.Remedy = fmt.Sprintf("`mgit sandbox daemons stop --repo-root %s` (it drains its sandboxes); any sandbox "+
		"command then starts the daemon from this install. Stop a running daemon before installing a new "+
		"release (docs/INSTALL-SANDBOX.md)", root)
	return r
}
