package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// worktreeCmd implements mgit worktree. Refs: FR-16, MGIT-8.3.1
func worktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage linked worktrees for multi-agent development",
	}

	// mgit worktree add
	var wtTaskID, wtAgentID, wtBranch string
	addCmd := &cobra.Command{
		Use:   "add [path]",
		Short: "Add a linked worktree bound to a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if wtTaskID == "" {
				return fmt.Errorf("--task-id is required")
			}
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			// Same lock boundary as `mgit work`: the lifetime lock is detached and
			// re-acquired around the shared-store phase only, so one agent's
			// materialization cannot starve the others. Refs: MGIT-120, ADR-009
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() }).
				WithSync(app.Sync, app.Repo, gitstore.NewCommitStore(app.Repo)).
				WithLocker(app.DetachLock())

			wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
				Path: args[0], TaskID: wtTaskID, AgentID: wtAgentID, Branch: wtBranch,
			})
			if err != nil {
				return fmt.Errorf("worktree add: %w", err)
			}
			_, _ = fmt.Fprintf(os.Stdout, "Created worktree %s -> task %s (branch %s)\n", wt.Path, wt.TaskID, wt.Branch)
			// `worktree add` is plumbing (mirrors `git worktree add`) and launches
			// no sandbox, so the honest-open posture applies: no fail-closed routing
			// wiring is installed. Use `mgit work --sandbox` for containment. MGIT-47
			injectAgentAdapters(os.Stderr, wt.Path, false)
			return nil
		},
	}
	bindTaskIDFlag(addCmd, &wtTaskID, "Task ID to bind (required)")
	addCmd.Flags().StringVar(&wtAgentID, "agent-id", "", "Agent ID")
	addCmd.Flags().StringVar(&wtBranch, "branch", "", "Branch name (default: task/<task-id>)")

	// mgit worktree list
	var porcelainList, listJSON bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List linked worktrees (a row whose directory is gone is marked prunable)",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() })

			wts, err := wtSvc.List(ctx)
			if err != nil {
				return err
			}
			return writeWorktreeList(os.Stdout, wts, porcelainList, listJSON)
		},
	}
	listCmd.Flags().BoolVar(&porcelainList, "porcelain", false, "Machine-readable output (a prunable row ends with the word)")
	listCmd.Flags().BoolVar(&listJSON, "json", false, "output as JSON (prunable is a field)")

	// mgit worktree remove
	var wtForce bool
	removeCmd := &cobra.Command{
		Use:   "remove [path]",
		Short: "Remove a linked worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() })

			if err := wtSvc.Remove(ctx, args[0], wtForce); err != nil {
				return fmt.Errorf("worktree remove: %w", err)
			}
			_, _ = fmt.Fprintf(os.Stdout, "Removed worktree %s\n", args[0])
			return nil
		},
	}
	removeCmd.Flags().BoolVar(&wtForce, "force", false, "Force remove even with uncommitted changes")

	// mgit worktree prune
	var wtDryRun bool
	pruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove stale worktree metadata",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, gitstore.NewWorktreeStore(app.Repo), func() time.Time { return time.Now().UTC() })

			stale, err := wtSvc.Prune(ctx, wtDryRun, 0)
			if err != nil {
				return fmt.Errorf("worktree prune: %w", err)
			}
			if wtDryRun {
				for _, p := range stale {
					_, _ = fmt.Fprintf(os.Stdout, "Would remove: %s\n", p)
				}
			} else {
				for _, p := range stale {
					_, _ = fmt.Fprintf(os.Stdout, "Removed: %s\n", p)
				}
			}
			if len(stale) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "No stale worktrees")
			}
			return nil
		},
	}
	pruneCmd.Flags().BoolVar(&wtDryRun, "dry-run", false, "Show what would be removed without removing")

	cmd.AddCommand(addCmd, listCmd, removeCmd, pruneCmd)
	return cmd
}

// writeWorktreeList renders the registry: one row per worktree, a trailing
// `prunable` (git's word) on a row whose directory is gone, and one line
// saying how to clear those — the registry otherwise shows a stale binding
// as a live one and gives a reader no reason to run prune. Refs: MGIT-194
func writeWorktreeList(w io.Writer, wts []model.WorktreeInfo, porcelain, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(wts)
	}
	if porcelain {
		for _, wt := range wts {
			_, _ = fmt.Fprintf(w, "%s [%s] %s%s\n", wt.Path, wt.Branch, wt.TaskID, prunableWord(wt, " prunable"))
		}
		return nil
	}
	if len(wts) == 0 {
		_, _ = fmt.Fprintln(w, "No linked worktrees")
		return nil
	}
	prunable := 0
	for _, wt := range wts {
		if wt.Prunable {
			prunable++
		}
		_, _ = fmt.Fprintf(w, "%-30s %s\t%s%s\n", wt.Path, wt.TaskID, wt.Branch, prunableWord(wt, "\tprunable"))
	}
	if prunable > 0 {
		_, _ = fmt.Fprintf(w, "%d prunable: the directory is gone but the registration remains — "+
			"`mgit worktree prune` clears it (--dry-run lists them first)\n", prunable)
	}
	return nil
}

// prunableWord is the marker for a prunable row, or nothing.
func prunableWord(wt model.WorktreeInfo, word string) string {
	if wt.Prunable {
		return word
	}
	return ""
}
