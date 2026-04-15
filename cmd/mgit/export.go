package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
)

// exportCmd implements mgit export. Refs: FR-8.13, MGIT-4.2.4
func exportCmd() *cobra.Command {
	var taskID, output, format string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export task data as JSON, git format-patch, or audit-log",
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
			data, err := buildExportPayload(ctx, app, format, taskID)
			if err != nil {
				return err
			}

			if output != "" {
				if err := os.WriteFile(output, data, 0o600); err != nil {
					return fmt.Errorf("export write: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Exported %s to %s\n", format, output)
				return nil
			}
			_, _ = os.Stdout.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Task to export (required)")
	cmd.Flags().StringVar(&output, "output", "", "Output file (default: stdout)")
	cmd.Flags().StringVar(&format, "format", "json", "Export format: json | git | audit-log")
	return cmd
}

// buildExportPayload renders the requested export format for a task.
// Refs: FR-8.13, MGIT-4.2.4
func buildExportPayload(ctx context.Context, app *App, format, taskID string) ([]byte, error) {
	switch format {
	case "json", "":
		records, err := app.Commit.GetTaskCommits(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("export json: %w", err)
		}
		data, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("export json marshal: %w", err)
		}
		return data, nil

	case "git":
		// Squash the task in dry-run mode so the export does not mutate state,
		// then render the result as a git format-patch.
		squashed, err := app.Squash.SquashTask(ctx, service.SquashRequest{
			TaskID: taskID,
			DryRun: true,
		})
		if err != nil {
			return nil, fmt.Errorf("export git: %w", err)
		}
		patch := app.Squash.ExportToGitPatch(squashed)
		return []byte(patch), nil

	case "audit-log":
		data, err := app.Audit.ExportAuditLog(service.AuditFilters{TaskID: taskID})
		if err != nil {
			return nil, fmt.Errorf("export audit-log: %w", err)
		}
		return data, nil

	default:
		return nil, fmt.Errorf("export: unknown format %q (want json|git|audit-log)", format)
	}
}
