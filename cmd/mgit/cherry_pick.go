package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
)

// cherryPickCmd implements mgit cherry-pick. The task ID for the new commit
// is, by default, DERIVED from the source commit's provenance (its
// [MGIT:TASK_ID] tag, now surfaced on read per MGIT-19) and may be overridden
// with --task-id. Cherry-picking a commit with no derivable task fails with a
// clear message rather than the opaque `invalid task ID: ""`.
// Refs: FR-8.16, MGIT-4.2.7, MGIT-19
func cherryPickCmd() *cobra.Command {
	var noCommit bool
	var onto, taskID string

	cmd := &cobra.Command{
		Use:   "cherry-pick [commit-hash]",
		Short: "Apply changes from a specific commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// --onto switches the shared HEAD; from a linked worktree that would
			// corrupt the parent's HEAD while the pick still lands on the bound
			// branch via the override. Reject it in a worktree. Refs: MGIT-24
			if onto != "" && app.BoundTask != "" {
				return fmt.Errorf("cannot cherry-pick --onto from a linked worktree (bound to task %s)", app.BoundTask)
			}

			// --onto: switch to the target branch via the CHECKOUT service so the
			// working tree is materialized to the target (and dirty trees are
			// blocked) — a bare ref switch would leave disk reflecting the old
			// branch while the content-applying pick writes into it (MGIT-54 M5).
			if onto != "" {
				if _, err := app.Checkout.Checkout(ctx, onto); err != nil {
					return fmt.Errorf("cherry-pick --onto: %w", err)
				}
			}

			// --no-commit: print what would be cherry-picked without creating a commit.
			if noCommit {
				source, err := app.Commit.GetCommit(ctx, args[0])
				if err != nil {
					return fmt.Errorf("cherry-pick: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "[no-commit] Would cherry-pick %s: %s\n", source.ShortID(), source.Message)
				return nil
			}

			// The pick itself (content-applying, conflict-safe -- MGIT-54)
			// lives in the service layer.
			c, err := app.Commit.CherryPick(ctx, service.CherryPickRequest{
				SourceHash: args[0],
				TaskID:     taskID,
			})
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(os.Stdout, "[%s] cherry-picked from %s\n", c.ShortID(), args[0])
			return nil
		},
	}

	cmd.Flags().BoolVar(&noCommit, "no-commit", false, "Print what would be cherry-picked without committing")
	cmd.Flags().StringVar(&onto, "onto", "", "Switch to branch before cherry-picking")
	bindTaskIDFlag(cmd, &taskID, "Override the task ID for the cherry-picked commit (default: derived from source)")
	return cmd
}
