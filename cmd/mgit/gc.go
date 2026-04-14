package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/service"
)

// gcCmd implements mgit gc. Refs: FR-8.4, FR-13.2, MGIT-4.2.11
func gcCmd() *cobra.Command {
	var aggressive, formatJSON bool
	var threshold int

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collection — pack loose objects and report stats",
		RunE: func(_ *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()
			result, err := app.GC.Run(ctx, service.GCRequest{
				Aggressive:    aggressive,
				PackThreshold: threshold,
			})
			if err != nil {
				return fmt.Errorf("gc: %w", err)
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			_, _ = fmt.Fprintf(os.Stdout,
				"%s: %d → %d loose objects, %d bytes saved\n",
				result.Status, result.LooseBefore, result.LooseAfter, result.BytesSaved)
			return nil
		},
	}

	cmd.Flags().BoolVar(&aggressive, "aggressive", false, "Force a full repack of all objects")
	cmd.Flags().IntVar(&threshold, "pack-threshold", service.DefaultGCPackThreshold,
		"Loose-object count above which packing runs (default 1000)")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
