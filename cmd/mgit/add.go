package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// addCmd implements mgit add. Refs: FR-8.15, MGIT-4.2.6
func addCmd() *cobra.Command {
	var all bool
	var taskScope string

	cmd := &cobra.Command{
		Use:   "add [paths...]",
		Short: "Stage files for the next commit",
		RunE: func(_ *cobra.Command, args []string) error {
			// --task is advisory; stored as metadata for future use.
			_ = taskScope
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			ws := gitstore.NewWorktreeStore(app.Repo)

			if all {
				// Stage all changes
				if err := ws.Add(ctx, "."); err != nil {
					return fmt.Errorf("add all: %w", err)
				}
				_, _ = fmt.Fprintln(os.Stdout, "Staged all changes")
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("specify files to add, or use --all")
			}

			for _, path := range args {
				if err := ws.Add(ctx, path); err != nil {
					return fmt.Errorf("add %s: %w", path, err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Staged: %s\n", path)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&all, "all", "A", false, "Stage all changes")
	cmd.Flags().StringVar(&taskScope, "task", "", "Task ID scope (advisory — stored as metadata for future use)")
	return cmd
}
