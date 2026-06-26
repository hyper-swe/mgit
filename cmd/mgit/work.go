package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// DESIGN: `mgit work` is delivered as a NEW first-class porcelain command
// rather than overloading `mgit worktree add`. `worktree add` is plumbing
// that mirrors `git worktree add`; "start an agent on a task" is a distinct,
// higher-level intent (create+bind the mgit worktree, write the FULL agent
// wiring including the CLAUDE.md sandbox-env block, and optionally launch the
// task sandbox). Keeping it separate leaves the plumbing command clean and
// gives agents one obvious entry point that closes dogfood gap #5: one task
// <-> one mgit worktree <-> one agent, commits auto-tagged (FR-16).
// Refs: MGIT-34, MGIT-26, FR-16

// workOptions are the parsed `mgit work` inputs. Refs: MGIT-34
type workOptions struct {
	Path          string
	TaskID        string
	AgentID       string
	Branch        string
	Base          string   // --base: pin the task fork-base to an explicit ref (ADR-008 §4)
	LaunchSandbox bool     // --sandbox: also launch the bound task sandbox
	Image         string   // --image: digest-pinned image (sandbox leg only)
	Network       string   // --network: none | allowlist | open
	Allow         []string // --allow: allowlist entries (allowlist mode)
}

// workDeps are the injected collaborators of workSetup, so the host-side
// orchestration is unit-testable without a real repo, daemon, or KVM.
// Refs: MGIT-34
type workDeps struct {
	addWorktree    func(ctx context.Context, opts model.WorktreeAddOptions) (*model.WorktreeInfo, error)
	writeAdapters  func(warn io.Writer, worktreePath string)
	upsertEnvDoc   func(worktreePath, taskID string) error
	connect        connectFunc
	mgitBinForDocs string
}

// newWorkCmd builds `mgit work` around an injected runner so the CLI parsing
// and required-flag validation are testable without opening a real app.
// Refs: MGIT-34
func newWorkCmd(run func(ctx context.Context, app *App, opts workOptions) error) *cobra.Command {
	var opts workOptions
	cmd := &cobra.Command{
		Use:   "work [path]",
		Short: "Start an agent on a task inside an mgit-managed worktree",
		Long: "Provisions an mgit worktree bound to the task (FR-16), wires the " +
			"agent-integration files (CLAUDE.md + .claude/settings.json) so the " +
			"agent's shell routes through `mgit run` into the task sandbox, and " +
			"optionally (--sandbox) launches that sandbox. If the sandbox backend " +
			"is unavailable the worktree and wiring still succeed; only the " +
			"sandbox leg fails-closed (NFR-17.6).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.TaskID == "" {
				return fmt.Errorf("--task is required")
			}
			opts.Path = args[0]
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()
			return run(cmd.Context(), app, opts)
		},
	}
	bindWorkFlags(cmd, &opts)
	return cmd
}

// bindWorkFlags registers the `mgit work` flags. Refs: MGIT-34
func bindWorkFlags(cmd *cobra.Command, opts *workOptions) {
	cmd.Flags().StringVar(&opts.TaskID, "task", "", "task ID to bind the worktree to (required)")
	cmd.Flags().StringVar(&opts.AgentID, "agent-id", "", "agent ID recorded with the worktree")
	cmd.Flags().StringVar(&opts.Branch, "branch", "", "branch name (default: task/<task-id>)")
	cmd.Flags().StringVar(&opts.Base, "base", "", "pin the task's fork-base to an explicit commit/ref (default: current local working state)")
	cmd.Flags().BoolVar(&opts.LaunchSandbox, "sandbox", false, "also launch the task's microVM sandbox (requires --image)")
	cmd.Flags().StringVar(&opts.Image, "image", "", "digest-pinned image <name>@sha256:<hex> (with --sandbox)")
	cmd.Flags().StringVar(&opts.Network, "network", model.NetworkModeNone, "sandbox network mode: none | allowlist | open")
	cmd.Flags().StringArrayVar(&opts.Allow, "allow", nil, "allowlist entry (repeatable; allowlist mode only)")
}

// workCmd is the production `mgit work` command. Refs: MGIT-34
func workCmd() *cobra.Command {
	return newWorkCmd(runWork)
}

// runWork is the production runner: it wires the App's worktree service into
// workDeps and invokes the orchestrator. Refs: MGIT-34
func runWork(ctx context.Context, app *App, opts workOptions) error {
	wtSvc := service.NewWorktreeService(app.Index, app.Branch,
		gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() }).
		WithSync(app.Sync, app.Repo, gitstore.NewCommitStore(app.Repo))
	deps := workDeps{
		addWorktree:    wtSvc.Add,
		writeAdapters:  injectAgentAdapters,
		upsertEnvDoc:   upsertWorktreeEnvDoc,
		connect:        productionSandboxConnect,
		mgitBinForDocs: currentMgitBin(),
	}
	_, err := workSetup(ctx, os.Stdout, deps, opts)
	return err
}

// workSetup runs the three independently fail-safe legs of the agent entry
// flow: (1) create+bind the mgit worktree (hard requirement — failure
// aborts); (2) write the agent-integration wiring (best-effort warn); (3)
// optionally launch the bound sandbox (gated, degrades gracefully). The
// returned WorktreeInfo is the created worktree; an error is returned only
// when the worktree itself could not be provisioned. Refs: MGIT-34, FR-16
func workSetup(ctx context.Context, out io.Writer, deps workDeps, opts workOptions) (*model.WorktreeInfo, error) {
	wt, err := deps.addWorktree(ctx, model.WorktreeAddOptions{
		Path: opts.Path, TaskID: opts.TaskID, AgentID: opts.AgentID, Branch: opts.Branch, Base: opts.Base,
	})
	if err != nil {
		return nil, fmt.Errorf("work: create worktree: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Created worktree %s -> task %s (branch %s)\n", wt.Path, wt.TaskID, wt.Branch)

	deps.writeAdapters(out, wt.Path)
	if envErr := deps.upsertEnvDoc(wt.Path, wt.TaskID); envErr != nil {
		_, _ = fmt.Fprintf(out, "warning: could not write CLAUDE.md sandbox section (%v)\n", envErr)
	}
	_, _ = fmt.Fprintf(out, "Wired agent routing (CLAUDE.md + .claude/settings.json -> mgit run)\n")

	if opts.LaunchSandbox {
		launchWorkSandbox(ctx, out, deps, opts, wt)
	} else {
		_, _ = fmt.Fprintf(out, "Sandbox not launched (run `mgit sandbox launch --task %s "+
			"--worktree %s --image <ref>` when ready, or pass --sandbox)\n", wt.TaskID, wt.Path)
	}
	return wt, nil
}

// launchWorkSandbox runs the optional sandbox-launch leg. It degrades
// gracefully: any failure (missing image, unavailable daemon/backend) is
// reported with a clear remedy and does NOT fail the worktree+wiring flow —
// the sandbox-routing leg simply fails-closed (NFR-17.6). On success the
// CLAUDE.md env block is regenerated to the live network posture (MGIT-11.11.2).
// Refs: MGIT-34, NFR-17.6
func launchWorkSandbox(ctx context.Context, out io.Writer, deps workDeps, opts workOptions, wt *model.WorktreeInfo) {
	if opts.Image == "" {
		_, _ = fmt.Fprintf(out, "sandbox not launched: --image is required with --sandbox; "+
			"run `mgit sandbox launch --task %s --worktree %s --image <ref>`\n", wt.TaskID, wt.Path)
		return
	}
	cl, err := deps.connect(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "sandbox not launched (%v); the worktree and agent wiring are ready — "+
			"run `mgit sandbox launch --task %s --worktree %s --image %s` once the backend is available\n",
			err, wt.TaskID, wt.Path, opts.Image)
		return
	}
	info, err := cl.Launch(ctx, model.SandboxLaunchOptions{
		TaskID: wt.TaskID, WorktreePath: wt.Path, ImageRef: opts.Image,
		Network: model.NetworkPolicy{Mode: opts.Network, Allowlist: opts.Allow},
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "sandbox not launched (%v); run `mgit sandbox launch --task %s "+
			"--worktree %s --image %s` to retry\n", err, wt.TaskID, wt.Path, opts.Image)
		return
	}
	writeSandboxEnvDoc(out, info)
	_, _ = fmt.Fprintf(out, "Launched sandbox %s for task %s (%s)\n", info.ID, info.TaskID, info.State)
}

// upsertWorktreeEnvDoc writes the worktree's CLAUDE.md sandbox-env block from
// only the task binding (no live sandbox yet), so the agent's knowledge layer
// describes the microVM/identical-path-mount posture from the moment the
// worktree is created. It fails safe to "no network" until a sandbox is
// launched (which regenerates the block to the live posture). Refs: MGIT-34, MGIT-11.11.2
func upsertWorktreeEnvDoc(worktreePath, _ string) error {
	return agentadapter.UpsertClaudeMd(worktreePath, agentadapter.SandboxEnv{
		WorktreePath: worktreePath,
		NetworkMode:  model.NetworkModeNone,
	})
}
