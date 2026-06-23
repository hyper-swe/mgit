package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// sandboxClient is the control-plane client surface the sandbox commands
// drive. *sandboxd.Client satisfies it; tests inject a fake. The CLI talks
// to the daemon ONLY through this — never the Store or Manager.
// Refs: FR-17.34, MGIT-11.10.9
type sandboxClient interface {
	Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error)
	Exec(ctx context.Context, taskID string, req model.ExecRequest, stdout, stderr io.Writer) (int, error)
	List(ctx context.Context) ([]model.SandboxInfo, error)
	Status(ctx context.Context, taskID string) (*model.SandboxInfo, error)
	Remove(ctx context.Context, taskID string, force bool) error
	Land(ctx context.Context, taskID string) (*controlproto.LandResult, error)
	// Grants lists a task's pending capability requests; Grant approves one by
	// its host-observed key (the deny->prompt->grant flow, FR-17.12).
	Grants(ctx context.Context, taskID string) ([]controlproto.PendingGrant, error)
	Grant(ctx context.Context, taskID, key string) (*controlproto.GrantResult, error)
	// Shell attaches an interactive session to a task's sandbox (T2
	// confined-agent, MGIT-11.11.4), proxying the supplied stdin/stdout/
	// stderr and returning the session exit code.
	Shell(ctx context.Context, taskID string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// connectFunc resolves a live daemon (activation + greeting-verified) and
// returns a client for it, or a clear error when the backend is
// unavailable — never a silent local fallback (running untrusted task work
// outside the sandbox would defeat FR-17 containment). Injected so the
// commands are testable without spawning a real daemon.
type connectFunc func(ctx context.Context) (sandboxClient, error)

// exitError carries a child exit status out of a command so main can
// propagate it as the process exit code. It is silenced (SilenceErrors) so
// the bare status never prints as an "Error:" line.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// sandboxCmd is the production `mgit sandbox` command group.
// Refs: FR-17, MGIT-11.10.9
func sandboxCmd() *cobra.Command {
	return newSandboxCmd(productionSandboxConnect)
}

// newSandboxCmd builds the command group around an injected connector.
func newSandboxCmd(connect connectFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Run task work in a hardware-isolated microVM sandbox (FR-17)",
	}
	cmd.AddCommand(
		sandboxLaunchCmd(connect),
		sandboxExecCmd(connect),
		sandboxLandCmd(connect),
		sandboxListCmd(connect),
		sandboxStatusCmd(connect),
		sandboxRemoveCmd(connect),
		sandboxGrantsCmd(connect),     // list pending capability requests (deny->prompt, MGIT-11.9.4)
		sandboxGrantCmd(connect),      // approve one pending capability request
		sandboxShellCmd(connect),      // T2 confined-agent interactive attach (MGIT-11.11.4)
		sandboxImageCmd(),             // host-local image registry (no daemon)
		sandboxClaudeHookCmd(connect), // hidden: Claude Code PreToolUse hook (MGIT-11.11.1)
	)
	return cmd
}

// sandboxLandCmd pulls a task's guest commits over the land channel and
// imports them host-side through the verified land path. Refs: FR-17.5, MGIT-11.10.10
func sandboxLandCmd(connect connectFunc) *cobra.Command {
	var task string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "land --task <id>",
		Short: "Land a task's sandbox commits onto the host branch (verified host-side)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if task == "" {
				return fmt.Errorf("--task is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.Land(cmd.Context(), task)
			if err != nil {
				return err
			}
			return writeLandResult(cmd.OutOrStdout(), res, asJSON, task)
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task ID whose sandbox commits to land (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// writeLandResult renders a land outcome as JSON or a human summary.
func writeLandResult(w io.Writer, res *controlproto.LandResult, asJSON bool, task string) error {
	if res == nil {
		res = &controlproto.LandResult{}
	}
	if asJSON {
		return json.NewEncoder(w).Encode(res)
	}
	if res.Commits == 0 {
		_, _ = fmt.Fprintf(w, "Nothing to land for task %s (no new commits)\n", task)
		return nil
	}
	_, _ = fmt.Fprintf(w, "Landed %d commit(s) for task %s onto %s\n", res.Commits, task, res.Branch)
	return nil
}

// sandboxLaunchCmd registers (lazily provisions) a sandbox for a task.
func sandboxLaunchCmd(connect connectFunc) *cobra.Command {
	var task, worktree, image, network string
	var allow []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "launch --task <id> --worktree <path> --image <ref>",
		Short: "Register a sandbox for a task (the VM boots on first exec)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if task == "" || worktree == "" || image == "" {
				return fmt.Errorf("--task, --worktree and --image are required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			info, err := cl.Launch(cmd.Context(), model.SandboxLaunchOptions{
				TaskID: task, WorktreePath: worktree, ImageRef: image,
				Network: model.NetworkPolicy{Mode: network, Allowlist: allow},
			})
			if err != nil {
				return err
			}
			// Regenerate the worktree's CLAUDE.md env section to match this
			// sandbox's network posture (MGIT-11.11.2).
			writeSandboxEnvDoc(cmd.ErrOrStderr(), info)
			return writeSandbox(cmd.OutOrStdout(), info, asJSON,
				fmt.Sprintf("Launched sandbox %s for task %s (%s)\n", info.ID, info.TaskID, info.State))
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task ID to bind (required)")
	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree path the sandbox mounts (required)")
	cmd.Flags().StringVar(&image, "image", "", "digest-pinned image reference <name>@sha256:<hex> (required)")
	cmd.Flags().StringVar(&network, "network", model.NetworkModeNone, "network mode: none | allowlist | open")
	cmd.Flags().StringArrayVar(&allow, "allow", nil, "allowlist entry (repeatable; allowlist mode only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// sandboxExecCmd runs one command inside a task's sandbox, streaming
// output and propagating the guest exit code.
func sandboxExecCmd(connect connectFunc) *cobra.Command {
	var task string
	var env []string
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
				return printErr(cmd.ErrOrStderr(), fmt.Errorf("--task is required"))
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			// argv is passed as a list — no shell on the host path — and only
			// the explicit --env injections are sent; the host environment is
			// never forwarded into the hostile guest (FR-17.3).
			code, err := cl.Exec(cmd.Context(), task,
				model.ExecRequest{Command: args, Env: env}, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task ID whose sandbox runs the command (required)")
	cmd.Flags().StringArrayVar(&env, "env", nil, "explicit KEY=VALUE injected into the guest (repeatable; host env is never forwarded)")
	return cmd
}

// sandboxShellCmd attaches an interactive session to a task's sandbox, the
// T2 fully-confined-agent attach surface (ADR-005, MGIT-11.11.4). It
// proxies stdin/stdout/stderr to the guest session and propagates the exit
// code. Fail-closed: an unavailable daemon is a clear error, never a local
// shell. Refs: MGIT-11.11.4
func sandboxShellCmd(connect connectFunc) *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:           "shell --task <id>",
		Short:         "Attach an interactive session to a task's sandbox (T2 confined agent)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if task == "" {
				return printErr(cmd.ErrOrStderr(), fmt.Errorf("--task is required"))
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			code, err := cl.Shell(cmd.Context(), task, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return printErr(cmd.ErrOrStderr(), err)
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task ID whose sandbox to attach (required)")
	return cmd
}

// sandboxListCmd lists registered sandboxes.
func sandboxListCmd(connect connectFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			list, err := cl.List(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			if len(list) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sandboxes")
				return nil
			}
			for _, sb := range list {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", sb.TaskID, sb.State, sb.ID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// sandboxStatusCmd shows the sandbox bound to a task.
func sandboxStatusCmd(connect connectFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status <task-id>",
		Short: "Show the sandbox bound to a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			info, err := cl.Status(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeSandbox(cmd.OutOrStdout(), info, asJSON,
				fmt.Sprintf("%s\t%s\t%s\n", info.TaskID, info.State, info.ID))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// sandboxRemoveCmd tears down a task's sandbox.
func sandboxRemoveCmd(connect connectFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <task-id>",
		Short: "Tear down a task's sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			if err := cl.Remove(cmd.Context(), args[0], force); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed sandbox for task %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "force teardown of a running VM")
	return cmd
}

// writeSandbox renders one sandbox as JSON or the supplied human line.
func writeSandbox(w io.Writer, info *model.SandboxInfo, asJSON bool, humanLine string) error {
	if asJSON {
		return json.NewEncoder(w).Encode(info)
	}
	_, _ = io.WriteString(w, humanLine)
	return nil
}

// printErr writes a clear error line and returns the error unchanged, so
// exec can surface failures itself under SilenceErrors.
func printErr(w io.Writer, err error) error {
	_, _ = fmt.Fprintf(w, "mgit sandbox: %v\n", err)
	return err
}
