package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// runCmd is the production `mgit run` command. It auto-routes a task
// command into the sandbox bound to the current worktree, fail-closed.
// Refs: FR-17.11, NFR-17.6, MGIT-11.11.5
func runCmd() *cobra.Command {
	return newRunCmd(productionSandboxConnect, os.Getwd)
}

// newRunCmd builds `mgit run` around an injected daemon connector and a
// cwd resolver, so the routing/fail-closed logic is unit-testable without
// a real daemon or a real working directory.
//
// `mgit run` is a PURE control-plane client: it resolves the cwd to its
// task-bound sandbox via List() and execs there. It has NO host-execution
// code path — if no sandbox is bound for the cwd or the daemon is
// unavailable, it errors and the command never runs on the host. That is
// fail-closed by construction (NFR-17.6), the property the seamless
// agent-integration hooks (MGIT-11.11.1, .3) rely on.
func newRunCmd(connect connectFunc, getwd func() (string, error)) *cobra.Command {
	var env []string
	var check bool
	cmd := &cobra.Command{
		Use:   "run [--env KEY=VALUE]... -- <command> [args...]",
		Short: "Run a command in the current worktree's task sandbox (fail-closed)",
		Long: "Routes a command into the microVM sandbox bound to the current " +
			"worktree's task. If no sandbox is bound or the daemon is unavailable, " +
			"the command is refused — it never runs on the host (fail-closed).",
		// Real errors are printed here; cobra must not also print them or
		// turn an exitError into an "Error:" line.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check {
				return runCheck(cmd, connect, getwd)
			}
			if len(args) == 0 {
				return printRunErr(cmd.ErrOrStderr(), fmt.Errorf("a command is required (mgit run -- <command>)"))
			}
			return runExec(cmd, connect, getwd, args, env)
		},
	}
	cmd.Flags().StringArrayVar(&env, "env", nil, "explicit KEY=VALUE injected into the guest (repeatable; host env is never forwarded)")
	cmd.Flags().BoolVar(&check, "check", false, "report whether a sandbox is available for the current worktree, without executing")
	return cmd
}

// runExec resolves the cwd's bound sandbox and runs the command in it,
// propagating the guest exit code. Host env is never forwarded; the guest
// cwd is the host cwd (identical-path mount). Refs: FR-17.3, FR-17.11
func runExec(cmd *cobra.Command, connect connectFunc, getwd func() (string, error), args, env []string) error {
	cl, dir, sb, err := resolveRun(cmd.Context(), connect, getwd)
	if err != nil {
		return printRunErr(cmd.ErrOrStderr(), err)
	}
	// argv as a list — no host shell — and only explicit --env injections;
	// the host environment is never forwarded into the hostile guest.
	code, err := cl.Exec(cmd.Context(), sb.TaskID,
		model.ExecRequest{Command: args, Dir: dir, Env: env}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return printRunErr(cmd.ErrOrStderr(), err)
	}
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// runCheck reports sandbox availability for the cwd without executing.
// Exit 0 (and an "available" line) when a sandbox is bound and the daemon
// is reachable; a non-zero error otherwise — the allow-vs-ask signal the
// MGIT-11.11.1 hook consumes. Refs: MGIT-11.11.5
func runCheck(cmd *cobra.Command, connect connectFunc, getwd func() (string, error)) error {
	_, _, sb, err := resolveRun(cmd.Context(), connect, getwd)
	if err != nil {
		return printRunErr(cmd.ErrOrStderr(), err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sandbox available for task %s (%s)\n", sb.TaskID, sb.State)
	return nil
}

// resolveRun connects to the daemon and resolves the cwd to its
// task-bound sandbox, returning the client, the cleaned cwd (the guest
// working directory) and the matched sandbox. Every failure mode —
// getwd, daemon-unavailable, list, no-match — is a hard error so callers
// never fall through to host execution. Refs: NFR-17.6, MGIT-11.11.5
func resolveRun(ctx context.Context, connect connectFunc, getwd func() (string, error)) (sandboxClient, string, *model.SandboxInfo, error) {
	dir, err := getwd()
	if err != nil {
		return nil, "", nil, fmt.Errorf("get working directory: %w", err)
	}
	// Canonical form on BOTH sides of the sandbox match: the record side
	// (work/launch) stores canonicalPath, so the runner's cwd must be
	// canonicalized too — on macOS the temp tree is reached through the
	// /var -> /private/var symlink, and a lexical-only comparison never
	// matches. Refs: MGIT-57
	dir = canonicalPath(dir)
	cl, err := connect(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	list, err := cl.List(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	sb := sandboxForDir(list, dir)
	if sb == nil {
		return nil, "", nil, fmt.Errorf("no sandbox bound for %s (run `mgit sandbox launch` first; commands never run on the host)", dir)
	}
	return cl, dir, sb, nil
}

// canonicalPath returns the absolute, symlink-resolved form of p — the ONE
// spelling both the sandbox records (work/launch) and the runner's cwd are
// reduced to before matching. Resolution is best-effort: if the path (or its
// tail) does not exist yet, the cleaned absolute form is returned.
// Refs: MGIT-57
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// sandboxForDir picks the live sandbox whose worktree is dir or the
// nearest ancestor of dir (longest matching path wins). Destroyed and
// landed sandboxes are not routable. Returns nil when none matches.
// Refs: MGIT-11.11.5
func sandboxForDir(list []model.SandboxInfo, dir string) *model.SandboxInfo {
	var best *model.SandboxInfo
	bestLen := -1
	for i := range list {
		sb := &list[i]
		if !routableState(sb.State) {
			continue
		}
		wt := filepath.Clean(sb.WorktreePath)
		if !dirWithin(dir, wt) {
			continue
		}
		if len(wt) > bestLen {
			best, bestLen = sb, len(wt)
		}
	}
	return best
}

// routableState reports whether a sandbox in this state can accept an
// exec (a created sandbox boots lazily on first exec, FR-17.10).
func routableState(state string) bool {
	switch state {
	case model.StateCreated, model.StateRunning, model.StateSuspended:
		return true
	default:
		return false
	}
}

// dirWithin reports whether dir equals base or is nested under it.
func dirWithin(dir, base string) bool {
	if dir == base {
		return true
	}
	return strings.HasPrefix(dir, base+string(filepath.Separator))
}

// printRunErr writes a clear error line and returns the error unchanged,
// so run can surface failures itself under SilenceErrors.
func printRunErr(w io.Writer, err error) error {
	_, _ = fmt.Fprintf(w, "mgit run: %v\n", err)
	return err
}
