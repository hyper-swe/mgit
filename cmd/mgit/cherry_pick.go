package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/service"
)

// cherryPickCmd implements mgit cherry-pick. Refs: FR-8.16, MGIT-4.2.7
func cherryPickCmd() *cobra.Command {
	var noCommit bool
	var onto string

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

			// --onto: switch to the target branch before cherry-picking.
			if onto != "" {
				if err := app.Branch.SwitchBranch(ctx, onto); err != nil {
					return fmt.Errorf("cherry-pick --onto: switch branch: %w", err)
				}
			}

			// Get the source commit
			source, err := app.Commit.GetCommit(ctx, args[0])
			if err != nil {
				return fmt.Errorf("cherry-pick: %w", err)
			}

			// --no-commit: print what would be cherry-picked without creating a commit.
			if noCommit {
				_, _ = fmt.Fprintf(os.Stdout, "[no-commit] Would cherry-pick %s: %s\n", source.ShortID(), source.Message)
				return nil
			}

			// Create a new commit with the same content on current branch
			c, err := app.Commit.CreateCommit(ctx, service.CreateCommitRequest{
				TaskID:    source.TaskID.String(),
				AgentID:   "mgit-cherry-pick",
				Message:   fmt.Sprintf("cherry-pick %s: %s", source.ShortID(), source.Message),
				FileDiffs: source.FileDiffs,
			})
			if err != nil {
				return fmt.Errorf("cherry-pick: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "[%s] cherry-picked from %s\n", c.ShortID(), source.ShortID())
			return nil
		},
	}

	cmd.Flags().BoolVar(&noCommit, "no-commit", false, "Print what would be cherry-picked without committing")
	cmd.Flags().StringVar(&onto, "onto", "", "Switch to branch before cherry-picking")
	return cmd
}
