package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// sandboxGrantsCmd lists a task's pending capability requests — egress
// destinations the host denied that the operator may approve for the sandbox's
// lifetime. The requests are host-derived from the observed denials (SEC-05),
// so the listing shows the real destination the host saw, never guest text.
// Refs: FR-17.12, MGIT-11.9.4
func sandboxGrantsCmd(connect connectFunc) *cobra.Command {
	var taskID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "grants --task <id>",
		Short: "List a sandbox's pending capability requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if taskID == "" {
				return fmt.Errorf("--task is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			pending, err := cl.Grants(cmd.Context(), taskID)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(pending)
			}
			if len(pending) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no pending capability requests")
				return nil
			}
			for _, p := range pending {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s:%d\t(key: %s)\n",
					p.Capability, p.DestIP, p.DestPort, p.Key)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&taskID, "task", "", "task whose pending requests to list (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// sandboxGrantCmd approves one pending capability request by its key (the
// host-observed "ip:port" for egress, from `mgit sandbox grants`). Approval
// widens the running sandbox's egress allowlist for its lifetime only and is
// recorded append-only; there is no allow-all. Refs: FR-17.12, SEC-05
func sandboxGrantCmd(connect connectFunc) *cobra.Command {
	var taskID string
	cmd := &cobra.Command{
		Use:   "grant --task <id> <key>",
		Short: "Approve one pending capability request (scoped to the sandbox lifetime)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if taskID == "" {
				return fmt.Errorf("--task is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			res, err := cl.Grant(cmd.Context(), taskID, args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "granted %s to %s:%d for this sandbox's lifetime\n",
				res.Capability, res.DestIP, res.DestPort)
			return nil
		},
	}
	cmd.Flags().StringVar(&taskID, "task", "", "task whose request to approve (required)")
	return cmd
}
