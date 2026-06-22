package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// checkoutCmd implements mgit checkout. Refs: FR-5.5, FR-5.5a, MGIT-4.2.9
func checkoutCmd() *cobra.Command {
	var formatJSON, create bool

	cmd := &cobra.Command{
		Use:   "checkout [branch]",
		Short: "Switch to a branch (use -b to create and switch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			ctx := context.Background()

			// -b/--create: git-familiar create-and-switch. Create a branch
			// with the literal name at current HEAD, then switch to it.
			if create {
				if _, err := app.Branch.CreateNamedBranch(ctx, args[0]); err != nil {
					return fmt.Errorf("checkout -b: %w", err)
				}
			}

			result, err := app.Checkout.Checkout(ctx, args[0])
			if err != nil {
				return err
			}

			if formatJSON {
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			_, _ = fmt.Fprintf(os.Stdout, "Switched to branch %s\n", result.Branch)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&create, "create", "b", false, "Create the branch at HEAD and switch to it")
	cmd.Flags().BoolVar(&formatJSON, "json", false, "Output as JSON")
	return cmd
}
