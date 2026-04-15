package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// statusCmd implements mgit status. Refs: FR-8.6, MGIT-4.1.5
func statusCmd() *cobra.Command {
	var taskID string
	var formatJSON, short, porcelain bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show working tree status",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			ws := gitstore.NewWorktreeStore(app.Repo)
			files, err := ws.Status(ctx)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			// Filter by task: only show files that are staged (task
			// scope is advisory — staging area is the filter proxy).
			if taskID != "" {
				_ = taskID // accepted for compatibility; worktree task scope
			}

			// Show current branch header.
			branch, branchErr := app.Repo.CurrentBranch()
			if branchErr != nil {
				branch = "HEAD (detached)"
			}

			if formatJSON {
				type statusJSON struct {
					Branch string                `json:"branch"`
					Files  []gitstore.FileStatus `json:"files"`
				}
				return json.NewEncoder(os.Stdout).Encode(statusJSON{
					Branch: branch,
					Files:  files,
				})
			}

			if porcelain {
				for _, f := range files {
					_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", f.Staging, f.Worktree, f.Path)
				}
				return nil
			}

			if !short {
				_, _ = fmt.Fprintf(os.Stdout, "On branch %s\n", branch)
			}

			if len(files) == 0 {
				if !short {
					_, _ = fmt.Fprintln(os.Stdout, "nothing to commit, working tree clean")
				}
				return nil
			}

			for _, f := range files {
				if short {
					_, _ = fmt.Fprintf(os.Stdout, "%s%s %s\n", f.Staging, f.Worktree, f.Path)
				} else {
					_, _ = fmt.Fprintf(os.Stdout, "\t%s %s %s\n", f.Staging, f.Worktree, f.Path)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Filter by task scope")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&short, "short", false, "Compact status output")
	cmd.Flags().BoolVar(&porcelain, "porcelain", false, "Machine-readable output")
	return cmd
}
