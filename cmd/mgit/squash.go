package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
)

// squashCmd implements mgit squash. Refs: FR-8.11, MGIT-4.2.2
func squashCmd() *cobra.Command {
	var taskID, message, toGitOutput string
	var dryRun, formatJSON, toGit, toMain, apply bool

	cmd := &cobra.Command{
		Use:   "squash",
		Short: "Squash micro-commits for a task",
		RunE: func(_ *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task-id is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// --apply implies --to-git behavior.
			if apply {
				toGit = true
			}

			squashed, err := app.Squash.SquashTask(ctx, service.SquashRequest{
				TaskID:  taskID,
				Message: message,
				DryRun:  dryRun,
			})
			if err != nil {
				return fmt.Errorf("squash: %w", err)
			}

			// --to-main: fast-forward merge the squash commit to main.
			if toMain && !dryRun {
				if err := app.Branch.SwitchBranch(ctx, "main"); err != nil {
					return fmt.Errorf("squash --to-main: switch to main: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Merged squash commit to main\n")
			}

			if toGit {
				patch := app.Squash.ExportToGitPatch(squashed)
				if toGitOutput != "" {
					if err := os.WriteFile(toGitOutput, []byte(patch), 0o600); err != nil {
						return fmt.Errorf("squash --to-git: write patch: %w", err)
					}
					_, _ = fmt.Fprintf(os.Stdout, "Wrote git patch to %s\n", toGitOutput)
				} else {
					_, _ = fmt.Fprint(os.Stdout, patch)
				}
				return nil
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(squashed)
			}

			if dryRun {
				_, _ = fmt.Fprintf(os.Stdout, "[dry-run] Would create squash commit:\n%s\n", squashed.Message)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", squashed.ShortID(), squashed.Message)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to squash (required)")
	cmd.Flags().StringVar(&message, "message", "", "Custom squash message")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview without making changes")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&toGit, "to-git", false, "Export squashed commit as git format-patch")
	cmd.Flags().StringVar(&toGitOutput, "to-git-output", "", "Write --to-git patch to file (default: stdout)")
	cmd.Flags().BoolVar(&toMain, "to-main", false, "Fast-forward merge squash commit to main branch")
	cmd.Flags().BoolVar(&apply, "apply", false, "Alias for --to-git that also writes the patch file")
	return cmd
}
