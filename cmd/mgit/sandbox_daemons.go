package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// tempRootGrace is how long a daemon may serve a temp-directory repository
// before it counts as a leak: an e2e or pair run is over in minutes, and the
// ones MGIT-191 found had run for eleven days. Refs: MGIT-191
const tempRootGrace = 24 * time.Hour

// daemonsDeps are the host facts the daemons verbs consult, injected so the
// verbs can be tested without a live host.
type daemonsDeps struct {
	list  func(ctx context.Context) ([]daemonrec.Listed, error)
	kill  func(pid int, sig syscall.Signal) error
	alive func(pid int) bool
	argv  func(pid int) (string, error) // the command line at a pid, for the identity check before a signal
	clock func() time.Time
}

func hostDaemonsDeps() daemonsDeps {
	return daemonsDeps{list: listHostDaemons, kill: syscall.Kill, alive: pidAlive, argv: pidArgv, clock: time.Now}
}

// pidArgv reads the command line at a pid through ps, which answers the same
// way on macOS and Linux; /proc is Linux-only. Refs: MGIT-191
func pidArgv(pid int) (string, error) {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // fixed binary, integer argument
	if err != nil {
		return "", fmt.Errorf("read the command line at pid %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isRecordedDaemon reports whether the process at the record's pid is the
// daemon the record describes: its command line names mgit-sandboxd and the
// record's socket. Pids are reused; a stale record plus a reused pid must
// never become a signal to an unrelated process. Refs: MGIT-191
func isRecordedDaemon(argv string, rec daemonrec.Record) bool {
	return strings.Contains(argv, "mgit-sandboxd") && strings.Contains(argv, rec.Socket)
}

// sameRoot matches a repository root as a path, not as bytes: a trailing
// slash or a symlinked prefix names the same repository. Refs: MGIT-166
func sameRoot(a, b string) bool {
	if a == b {
		return true
	}
	return rootPath(a) == rootPath(b)
}

func rootPath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// listHostDaemons reads every daemon record under this user's runtime base
// and judges each one. Refs: MGIT-191
func listHostDaemons(_ context.Context) ([]daemonrec.Listed, error) {
	base := filepath.Join(runtimeBase(), fmt.Sprintf("mgit-%d", os.Getuid()))
	recs, problems, err := daemonrec.List(base)
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%d daemon record(s) could not be read, starting with: %s", len(problems), problems[0])
	}
	now := time.Now()
	out := make([]daemonrec.Listed, 0, len(recs))
	for _, rec := range recs {
		out = append(out, daemonrec.Listed{Record: rec, Status: daemonrec.Classify(rec, now, pidAlive, tempRoots())})
	}
	return out, nil
}

// tempRoots names the directories under which a repository root is a
// scratch of some run rather than a repository anyone keeps.
func tempRoots() []string {
	return []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
}

// pidAlive asks the kernel, not ps.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// sandboxDaemonsCmd is the host-wide view MGIT-185 asked for: every sandbox
// daemon on this host with its root, age and what is wrong with it — without
// ps — and a stop scoped to one repository's daemon, never a blanket pkill:
// the same host runs the daemons of several real repositories. Refs: MGIT-191, MGIT-185
func sandboxDaemonsCmd(deps daemonsDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemons",
		Short: "List every sandbox daemon on this host, with its repository root, age and state",
		Long: "Reads the record each daemon keeps beside its socket and says, for every daemon this user " +
			"runs: pid, age, repository root, and flags — temp-root (a scratch of some run), root-gone " +
			"(the repository was deleted; the daemon drains itself within a poll), dead (a stale record, " +
			"pruned). Nothing here touches a daemon; `daemons stop --repo-root` does, by pid.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			listed, err := deps.list(cmd.Context())
			if err != nil {
				return err
			}
			return renderDaemons(cmd.OutOrStdout(), listed)
		},
	}
	cmd.AddCommand(sandboxDaemonsStopCmd(deps))
	return cmd
}

func renderDaemons(w io.Writer, listed []daemonrec.Listed) error {
	if len(listed) == 0 {
		_, err := fmt.Fprintln(w, "no sandbox daemons are recorded for this user on this host")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PID\tAGE\tVERSION\tROOT\tFLAGS")
	for _, d := range listed {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", d.Record.PID, shortAge(d.Status.Age),
			orDash(d.Record.Version), d.Record.RepoRoot, daemonFlags(d.Status))
	}
	return tw.Flush()
}

func daemonFlags(st daemonrec.Status) string {
	var flags []string
	if !st.Alive {
		flags = append(flags, "dead")
	}
	if st.RootGone {
		flags = append(flags, "root-gone")
	}
	if st.TempRoot {
		flags = append(flags, "temp-root")
	}
	if st.Leaked(tempRootGrace) {
		flags = append(flags, "LEAKED")
	}
	if len(flags) == 0 {
		return "-"
	}
	out := flags[0]
	for _, f := range flags[1:] {
		out += "," + f
	}
	return out
}

func shortAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sandboxDaemonsStopCmd stops the daemon serving one repository root, by the
// pid its record names, and waits for it to go. Refs: MGIT-191, MGIT-185
func sandboxDaemonsStopCmd(deps daemonsDeps) *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "stop --repo-root <path>",
		Short: "Stop the sandbox daemon serving one repository root, by its recorded pid",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				return errors.New("--repo-root is required: a stop is scoped to one repository, never the host")
			}
			listed, err := deps.list(cmd.Context())
			if err != nil {
				return err
			}
			for _, d := range listed {
				if !sameRoot(d.Record.RepoRoot, root) {
					continue
				}
				return stopDaemon(cmd.OutOrStdout(), deps, d)
			}
			return fmt.Errorf("no sandbox daemon is recorded for %s (see `mgit sandbox daemons`)", root)
		},
	}
	cmd.Flags().StringVar(&root, "repo-root", "", "repository root whose daemon to stop")
	return cmd
}

func stopDaemon(w io.Writer, deps daemonsDeps, d daemonrec.Listed) error {
	pid := d.Record.PID
	if !d.Status.Alive {
		_, _ = fmt.Fprintf(w, "daemon %d for %s is already gone; its record was stale\n", pid, d.Record.RepoRoot)
		return daemonrec.Remove(d.Record.Socket)
	}
	argv, err := deps.argv(pid)
	if err != nil {
		return fmt.Errorf("refusing to signal pid %d for %s: %w", pid, d.Record.RepoRoot, err)
	}
	if !isRecordedDaemon(argv, d.Record) {
		_ = daemonrec.Remove(d.Record.Socket) // the record is stale; the pid belongs to something else now
		return fmt.Errorf("refusing to signal pid %d for %s: it is not the daemon the record names — its command "+
			"line is %q; the stale record was removed and nothing was signaled", pid, d.Record.RepoRoot, argv)
	}
	if err := deps.kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon %d: %w", pid, err)
	}
	deadline := deps.clock().Add(10 * time.Second)
	for deps.alive(pid) {
		if deps.clock().After(deadline) {
			return fmt.Errorf("daemon %d for %s did not exit within 10s of SIGTERM; it is still draining or stuck — "+
				"inspect it before anything harsher", pid, d.Record.RepoRoot)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, _ = fmt.Fprintf(w, "stopped daemon %d for %s\n", pid, d.Record.RepoRoot)
	return nil
}
