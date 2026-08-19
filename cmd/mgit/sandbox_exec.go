// Package main's sandbox exec surface: `mgit sandbox exec`, its --timeout,
// and the failure path that decides WHO is at fault when a command produces
// no result. Split from sandbox.go, which the exec-liveness work pushed past
// the 500-line file limit. Refs: FR-17.11, FR-17.11.1, MGIT-133
package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// sandboxExecCmd runs one command inside a task's sandbox, streaming
// output and propagating the guest exit code.
func sandboxExecCmd(connect connectFunc) *cobra.Command {
	var task string
	var env []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "exec --task <id> -- <command> [args...]",
		Short: "Run a command in a task's sandbox (streams output, propagates exit code)",
		Args:  cobra.MinimumNArgs(1),
		// Real errors are printed here; cobra must not also print them or
		// turn an exitError into an "Error:" line.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if task == "" {
				return printErr(cmd.ErrOrStderr(), fmt.Errorf("--task-id is required"))
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			// argv is passed as a list — no shell on the host path — and only
			// the explicit --env injections are sent; the host environment is
			// never forwarded into the hostile guest (FR-17.3).
			code, err := cl.Exec(cmd.Context(), task,
				model.ExecRequest{Command: args, Env: env, Timeout: timeout},
				cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return execFailure(cmd, cl, task, err)
			}
			if code != 0 {
				// A signal death may be memory exhaustion; name the cap in
				// force so the caller does not "fix" its workload instead
				// (R-H212). Status is only consulted on the failure path.
				if info, sErr := cl.Status(cmd.Context(), task); sErr == nil {
					writeMemoryAdvisory(cmd.ErrOrStderr(), info, code)
				}
				return &exitError{code: code}
			}
			return nil
		},
	}
	bindTaskIDFlag(cmd, &task, "task ID whose sandbox runs the command (required)")
	cmd.Flags().StringArrayVar(&env, "env", nil, "explicit KEY=VALUE injected into the guest (repeatable; host env is never forwarded)")
	bindExecTimeoutFlag(cmd, &timeout)
	return cmd
}

// execFailure reports an exec that never produced a result, printing the ONE
// diagnosis its evidence supports.
//
// The sandbox is consulted for that diagnosis — a guest lost mid-command may
// have been killed by its own kernel for exceeding the cap in force (R-H212),
// and a guest that never started ran nothing at all and gets a different
// answer (MGIT-104). But a DAEMON that stopped answering is asked nothing: it
// has just demonstrated it cannot answer, so the question would hang for the
// whole control-plane timeout before yielding a diagnosis that never needed
// it. Refs: MGIT-133, MGIT-104, R-H212
func execFailure(cmd *cobra.Command, cl sandboxClient, task string, err error) error {
	if isDaemonStall(err) {
		defer writeGuestFailureAdvisory(cmd.Context(), cmd.ErrOrStderr(),
			&model.SandboxInfo{TaskID: task}, err)
		return printErr(cmd.ErrOrStderr(), err)
	}
	if info, sErr := cl.Status(cmd.Context(), task); sErr == nil {
		defer writeGuestFailureAdvisory(cmd.Context(), cmd.ErrOrStderr(), info, err)
	}
	return printErr(cmd.ErrOrStderr(), err)
}

// bindExecTimeoutFlag adds --timeout to a command that runs a guest command.
//
// It DEFAULTS TO UNBOUNDED, and that default is the point. A timeout that
// applies unasked is the defect MGIT-122 removed, just with a larger number:
// whatever value is chosen, some legitimate build exceeds it and dies for
// taking too long rather than for being stuck. What makes unbounded safe is
// the liveness beat, which bounds SILENCE — so a wedged daemon is caught in
// seconds without any opinion about how long a build may run. This flag is for
// the caller who genuinely wants a duration bound and asks for one.
// Refs: MGIT-133, MGIT-122, FR-17.11
func bindExecTimeoutFlag(cmd *cobra.Command, timeout *time.Duration) {
	cmd.Flags().DurationVar(timeout, "timeout", 0,
		"bound this command's total run time (e.g. 10m); default unbounded — "+
			"a stalled daemon is caught by liveness beats, not by capping the command")
}
