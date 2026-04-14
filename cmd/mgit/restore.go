package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// restoreCmd implements mgit restore. Refs: FR-6.7, MGIT-4.2.8
func restoreCmd() *cobra.Command {
	var commitHash string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "restore [file]",
		Short: "Restore a file from a specific commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if commitHash == "" {
				return fmt.Errorf("--commit is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			result, err := app.Restore.RestoreFile(ctx, args[0], commitHash)
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			short := commitHash
			if len(short) > 8 {
				short = short[:8]
			}
			_, _ = fmt.Fprintf(os.Stdout, "Restored %s (%d bytes) from commit %s\n",
				result.Path, result.BytesWrit, short)
			return nil
		},
	}

	cmd.Flags().StringVar(&commitHash, "commit", "", "Commit to restore from (required)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
