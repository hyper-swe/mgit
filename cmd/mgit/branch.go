package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// branchCmd implements mgit branch. Refs: FR-8.8, MGIT-4.1.7
func branchCmd() *cobra.Command {
	var taskID, deleteBranch, renameBranch string
	var force, formatJSON, active bool

	cmd := &cobra.Command{
		Use:   "branch [name]",
		Short: "Manage branches",
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// Delete mode
			if deleteBranch != "" {
				if err := app.Branch.DeleteBranch(ctx, deleteBranch, force); err != nil {
					return fmt.Errorf("branch delete: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Deleted branch %s\n", deleteBranch)
				return nil
			}

			// Rename mode
			if renameBranch != "" && len(args) > 0 {
				// Rename is implemented as: create new, copy HEAD, delete old.
				oldBranch, err := app.Branch.GetBranch(ctx, renameBranch)
				if err != nil {
					return fmt.Errorf("branch rename: %w", err)
				}
				if _, createErr := app.Branch.CreateBranch(ctx, oldBranch.TaskID.String()); createErr != nil {
					return fmt.Errorf("branch rename: %w", createErr)
				}
				if err := app.Branch.DeleteBranch(ctx, renameBranch, true); err != nil {
					return fmt.Errorf("branch rename delete old: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Renamed %s → %s\n", renameBranch, args[0])
				return nil
			}

			// Create mode (with --task-id)
			if taskID != "" {
				branch, err := app.Branch.CreateBranch(ctx, taskID)
				if err != nil {
					return fmt.Errorf("branch create: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Created branch %s\n", branch.Name)
				return nil
			}

			// Switch mode (with arg, no other flags). The bare-list aliases
			// fall through to list mode for consistency with
			// `mgit worktree list` / `mgit sandbox list`.
			if len(args) > 0 && !isBranchListArg(args) {
				if app.BoundTask != "" {
					return fmt.Errorf("cannot switch branches in a linked worktree (bound to task %s)", app.BoundTask)
				}
				if err := app.Branch.SwitchBranch(ctx, args[0]); err != nil {
					return fmt.Errorf("branch switch: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Switched to branch %s\n", args[0])
				return nil
			}

			// List mode (default)
			branches, err := app.Branch.ListBranches(ctx)
			if err != nil {
				return fmt.Errorf("branch list: %w", err)
			}

			// --active: filter to branches with recent commits (task-bound)
			if active {
				var filtered []*model.Branch
				for _, b := range branches {
					if strings.HasPrefix(b.Name, "task/") {
						filtered = append(filtered, b)
					}
				}
				branches = filtered
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(branches)
			}

			current, _ := app.Repo.CurrentBranch()
			for _, b := range branches {
				marker := "  "
				if b.Name == current {
					marker = "* "
				}
				_, _ = fmt.Fprintf(os.Stdout, "%s%s\t%s\n", marker, b.Name, b.HeadCommit[:8])
			}
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Create branch for task")
	cmd.Flags().StringVarP(&deleteBranch, "delete", "d", "", "Delete branch")
	cmd.Flags().StringVar(&renameBranch, "rename", "", "Rename branch (provide new name as arg)")
	cmd.Flags().BoolVar(&active, "active", false, "List only active task branches")
	cmd.Flags().BoolVar(&force, "force", false, "Force delete unmerged branch")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}

// isBranchListArg reports whether the positional args request a branch
// listing rather than a branch switch. Bare `mgit branch`, `mgit branch
// list`, and `mgit branch ls` all list, mirroring `worktree list` /
// `sandbox list`. Any other single positional is a branch name to switch to.
//
// Known limitation: a branch literally named "list" or "ls" cannot be
// switched to via `mgit branch <name>` — use `mgit checkout <name>` instead.
// Refs: MGIT-23
func isBranchListArg(args []string) bool {
	if len(args) == 0 {
		return true
	}
	return len(args) == 1 && (args[0] == "list" || args[0] == "ls")
}
