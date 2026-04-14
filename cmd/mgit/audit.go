package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/service"
)

// auditCmd implements mgit audit. Refs: FR-8.14, MGIT-4.2.5
func auditCmd() *cobra.Command {
	var taskID, agentID, since, until, opType, outputFormat string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "View audit trail",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			entries, err := app.Audit.GetAuditLog(service.AuditFilters{
				TaskID:    taskID,
				AgentID:   agentID,
				Operation: service.AuditOpType(opType),
				Since:     since,
				Until:     until,
			})
			if err != nil {
				return fmt.Errorf("audit: %w", err)
			}

			// --format=csv: output as CSV rows.
			if outputFormat == "csv" {
				_, _ = fmt.Fprintln(os.Stdout, "timestamp,operation,agent_id,task_id,commit_id")
				for _, e := range entries {
					_, _ = fmt.Fprintf(os.Stdout, "%s,%s,%s,%s,%s\n",
						e.Timestamp, e.Operation, e.AgentID, e.TaskID, e.CommitID)
				}
				return nil
			}

			if formatJSON || outputFormat == "json" {
				return json.NewEncoder(os.Stdout).Encode(entries)
			}

			if len(entries) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "No audit entries found")
				return nil
			}
			for _, e := range entries {
				_, _ = fmt.Fprintf(os.Stdout, "%s %s %s %s %s\n",
					e.Timestamp, e.Operation, e.AgentID, e.TaskID, e.CommitID)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&taskID, "task-id", "", "Filter by task ID")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Filter by agent ID")
	cmd.Flags().StringVar(&since, "since", "", "Show entries after date (RFC3339)")
	cmd.Flags().StringVar(&until, "until", "", "Show entries before date (RFC3339)")
	cmd.Flags().StringVar(&opType, "type", "", "Filter by operation type (e.g. CREATE_COMMIT, SQUASH)")
	cmd.Flags().StringVar(&outputFormat, "format", "", "Output format: json | csv")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
