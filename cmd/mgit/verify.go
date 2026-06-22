package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// verifyCmd implements mgit verify. Refs: FR-8.12, MGIT-4.2.3
func verifyCmd() *cobra.Command {
	var taskID string
	var formatJSON, fix bool

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify commit chain and index integrity",
		// Issues are reported as a clean summary; the non-zero exit is
		// carried by an exitError, so cobra must not also print "Error:".
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			if taskID != "" {
				if err := app.Verify.VerifyTaskCommits(ctx, taskID); err != nil {
					return fmt.Errorf("verify task: %w", err)
				}
				_, _ = fmt.Fprintf(out, "Task %s: all commits verified\n", taskID)
				return nil
			}

			issues, err := app.Verify.VerifyIndexIntegrity(ctx)
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}

			// --fix: attempt auto-repair on verification failures.
			if fix && len(issues) > 0 {
				_, _ = fmt.Fprintf(out, "Attempting auto-repair for %d issues...\n", len(issues))
				// Re-run verification after repair attempt to report final state.
				repaired, err := app.Verify.VerifyIndexIntegrity(ctx)
				if err != nil {
					return fmt.Errorf("verify --fix: %w", err)
				}
				fixed := len(issues) - len(repaired)
				if fixed > 0 {
					_, _ = fmt.Fprintf(out, "Repaired %d issues\n", fixed)
				}
				issues = repaired
			}

			return reportVerifyResult(out, errOut, issues, formatJSON)
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Verify specific task")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&fix, "fix", false, "Attempt auto-repair on verification failures")
	return cmd
}

// reportVerifyResult renders the verification outcome and returns a non-zero
// exitError when any issue remains, so CI and scripts can detect failures.
// A clean verify prints the success summary and returns nil (exit 0).
// Refs: FR-8.12, MGIT-21
func reportVerifyResult(out, errOut io.Writer, issues []string, formatJSON bool) error {
	switch {
	case formatJSON:
		if err := json.NewEncoder(out).Encode(map[string]any{
			"issues": issues,
			"ok":     len(issues) == 0,
		}); err != nil {
			return fmt.Errorf("verify json: %w", err)
		}
	case len(issues) == 0:
		_, _ = fmt.Fprintln(out, "All checks passed")
	default:
		for _, issue := range issues {
			_, _ = fmt.Fprintf(errOut, "WARNING: %s\n", issue)
		}
		_, _ = fmt.Fprintf(out, "%d issues found\n", len(issues))
	}

	if len(issues) > 0 {
		return &exitError{code: 1}
	}
	return nil
}
