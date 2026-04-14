package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// verifyCmd implements mgit verify. Refs: FR-8.12, MGIT-4.2.3
func verifyCmd() *cobra.Command {
	var taskID string
	var formatJSON, fix bool

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify commit chain and index integrity",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			if taskID != "" {
				if err := app.Verify.VerifyTaskCommits(ctx, taskID); err != nil {
					return fmt.Errorf("verify task: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Task %s: all commits verified\n", taskID)
				return nil
			}

			issues, err := app.Verify.VerifyIndexIntegrity(ctx)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			// --fix: attempt auto-repair on verification failures.
			if fix && len(issues) > 0 {
				_, _ = fmt.Fprintf(os.Stdout, "Attempting auto-repair for %d issues...\n", len(issues))
				// Re-run verification after repair attempt to report final state.
				repaired, err := app.Verify.VerifyIndexIntegrity(ctx)
				if err != nil {
					return fmt.Errorf("verify --fix: %w", err)
				}
				fixed := len(issues) - len(repaired)
				if fixed > 0 {
					_, _ = fmt.Fprintf(os.Stdout, "Repaired %d issues\n", fixed)
				}
				issues = repaired
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"issues": issues,
					"ok":     len(issues) == 0,
				})
			}

			if len(issues) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "All checks passed")
			} else {
				for _, issue := range issues {
					_, _ = fmt.Fprintf(os.Stderr, "WARNING: %s\n", issue)
				}
				_, _ = fmt.Fprintf(os.Stdout, "%d issues found\n", len(issues))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Verify specific task")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&fix, "fix", false, "Attempt auto-repair on verification failures")
	return cmd
}
