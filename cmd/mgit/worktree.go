package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
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
				return fmt.Errorf("--task is required")
			}
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, func() time.Time { return time.Now().UTC() })

			wt, err := wtSvc.Add(ctx, model.WorktreeAddOptions{
				Path: args[0], TaskID: wtTaskID, AgentID: wtAgentID, Branch: wtBranch,
			})
			if err != nil {
				return fmt.Errorf("worktree add: %w", err)
			}
			_, _ = fmt.Fprintf(os.Stdout, "Created worktree %s -> task %s (branch %s)\n", wt.Path, wt.TaskID, wt.Branch)
			return nil
		},
	}
	addCmd.Flags().StringVar(&wtTaskID, "task", "", "Task ID to bind (required)")
	addCmd.Flags().StringVar(&wtAgentID, "agent-id", "", "Agent ID")
	addCmd.Flags().StringVar(&wtBranch, "branch", "", "Branch name (default: task/<task-id>)")

	// mgit worktree list
	var porcelainList bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List linked worktrees",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, func() time.Time { return time.Now().UTC() })

			wts, err := wtSvc.List(ctx)
			if err != nil {
				return err
			}
			if porcelainList {
				for _, wt := range wts {
					_, _ = fmt.Fprintf(os.Stdout, "%s [%s] %s\n", wt.Path, wt.Branch, wt.TaskID)
				}
				return nil
			}
			if len(wts) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "No linked worktrees")
				return nil
			}
			for _, wt := range wts {
				_, _ = fmt.Fprintf(os.Stdout, "%-30s %s\t%s\n", wt.Path, wt.TaskID, wt.Branch)
			}
			return nil
		},
	}
	listCmd.Flags().BoolVar(&porcelainList, "porcelain", false, "Machine-readable output")

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
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, func() time.Time { return time.Now().UTC() })

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
			wtSvc := service.NewWorktreeService(app.Index, app.Branch, func() time.Time { return time.Now().UTC() })

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
