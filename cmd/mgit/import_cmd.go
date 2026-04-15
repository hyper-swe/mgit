package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
)

// importCmd implements mgit import. Refs: FR-12.5, MGIT-4.2.12
func importCmd() *cobra.Command {
	var file, mode string
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import an mgit bundle archive (verifies SHA-256 manifest)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}

			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			data, err := os.ReadFile(file) //nolint:gosec // user-supplied path
			if err != nil {
				return fmt.Errorf("import: read bundle: %w", err)
			}

			ctx := context.Background()
			result, err := app.Bundle.Import(ctx, data, service.ImportMode(mode))
			if err != nil {
				return err
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			_, _ = fmt.Fprintf(os.Stdout,
				"Imported %d records (%d skipped) from %s in %s mode\n",
				result.Imported, result.Skipped, file, result.Mode)
			return nil
		},
	}

	cmd.Flags().StringVar(&file, "file", "", "Bundle file to import (required)")
	cmd.Flags().StringVar(&mode, "mode", string(service.ImportMerge), "Import mode: merge | replace")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
