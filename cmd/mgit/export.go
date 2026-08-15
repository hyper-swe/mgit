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
		Use:   "export [task-id]",
		Short: "Export task data as JSON, git format-patch, or audit-log",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// A positional task ID is equivalent to --task-id; the positional
			// wins if both are supplied.
			taskID = firstNonEmpty(argAt(args, 0), taskID)
			if taskID == "" {
				return fmt.Errorf("a task ID (positional or --task-id) is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			payload, err := buildExportPayload(ctx, app, format, taskID)
			if err != nil {
				return err
			}

			// Operator notes go to stderr so stdout stays a clean, pipeable
			// artifact. Refs: MGIT-112
			if payload.note != "" {
				_, _ = fmt.Fprintln(os.Stderr, payload.note)
			}
			// Nothing to emit is NOT the same as an empty artifact: writing an
			// empty patch file (or printing one) is the silent-loss shape this
			// verb is being fixed for. The note above already said what
			// happened. Refs: MGIT-112
			if len(payload.data) == 0 {
				return nil
			}

			if output != "" {
				if err := os.WriteFile(output, payload.data, 0o600); err != nil {
					return fmt.Errorf("export write: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Exported %s to %s\n", format, output)
				return nil
			}
			_, _ = os.Stdout.Write(payload.data)
			if payload.data[len(payload.data)-1] != '\n' {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			return nil
		},
	}

	bindTaskIDFlag(cmd, &taskID, "Task to export (required)")
	cmd.Flags().StringVar(&output, "output", "", "Output file (default: stdout)")
	cmd.Flags().StringVar(&format, "format", "json", "Export format: json | git | audit-log")
	return cmd
}

// exportPayload is a rendered export: the bytes destined for stdout (or
// --output), plus an operator-facing note that must NOT be mixed into them.
// Empty data means there is deliberately nothing to emit, and note says why.
// Refs: FR-8.13, MGIT-112
type exportPayload struct {
	data []byte
	note string
}

// buildExportPayload renders the requested export format for a task. Every
// format here is a READ: none of them creates a commit, moves a ref, writes an
// index row or appends to the audit trail. Refs: FR-8.13, MGIT-4.2.4, MGIT-112
func buildExportPayload(ctx context.Context, app *App, format, taskID string) (exportPayload, error) {
	switch format {
	case "json", "":
		// Reads the task's indexed micro-commits straight from SQLite — no
		// squash involved, so it never depended on the state MGIT-112 was
		// missing.
		records, err := app.Commit.GetTaskCommits(ctx, taskID)
		if err != nil {
			return exportPayload{}, fmt.Errorf("export json: %w", err)
		}
		data, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return exportPayload{}, fmt.Errorf("export json marshal: %w", err)
		}
		return exportPayload{data: data}, nil

	case "git":
		return exportGitPatch(ctx, app, taskID)

	case "audit-log":
		// Also a straight SQLite read of the append-only audit trail — no
		// squash state dependency.
		data, err := app.Audit.ExportAuditLog(service.AuditFilters{TaskID: taskID})
		if err != nil {
			return exportPayload{}, fmt.Errorf("export audit-log: %w", err)
		}
		return exportPayload{data: data}, nil

	default:
		return exportPayload{}, fmt.Errorf("export: unknown format %q (want json|git|audit-log)", format)
	}
}

// exportGitPatch renders the task's net change as a git format-patch without
// mutating anything. A genuinely empty net change yields no patch bytes and an
// explicit note instead, so a reviewer mid-recovery can tell "the work
// canceled out" from "the tool failed" — an uncomputable diff comes back as an
// error and exits non-zero. Refs: FR-7, MGIT-112
func exportGitPatch(ctx context.Context, app *App, taskID string) (exportPayload, error) {
	preview, err := app.Squash.PreviewGitPatch(ctx, service.SquashRequest{TaskID: taskID})
	if err != nil {
		return exportPayload{}, fmt.Errorf("export git: %w", err)
	}
	if preview.Empty {
		return exportPayload{note: emptyNetChangeNote(taskID)}, nil
	}
	return exportPayload{data: []byte(preview.Patch)}, nil
}

// emptyNetChangeNote is the operator-facing line every patch-emitting verb
// prints when a task's commits cancel out against its base. Saying it out loud
// is what makes a legitimate empty result distinguishable from a failure — a
// reviewer mid-recovery must never have to guess which one they are looking at.
// Refs: MGIT-112
func emptyNetChangeNote(taskID string) string {
	return fmt.Sprintf(
		"note: task %s has an EMPTY net change — its commits cancel out against its "+
			"base, so there is nothing to export and no patch was written.\n"+
			"  This is not a failure. Review the steps with `mgit log --oneline` and "+
			"`mgit diff --task-id %s`.", taskID, taskID)
}
