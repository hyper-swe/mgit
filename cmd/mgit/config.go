package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// configCmd implements mgit config. Refs: FR-8.9, MGIT-4.1.8
func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage mgit configuration",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "get [key]",
			Short: "Get a config value",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				val, err := app.Config.Get(args[0])
				if err != nil {
					return fmt.Errorf("config get: %w", err)
				}
				_, _ = fmt.Fprintln(os.Stdout, val)
				return nil
			},
		},
		&cobra.Command{
			Use:   "set [key] [value]",
			Short: "Set a config value",
			Args:  cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				if err := app.Config.Set(args[0], args[1]); err != nil {
					return fmt.Errorf("config set: %w", err)
				}
				if err := app.Config.Save(); err != nil {
					return fmt.Errorf("config save: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "%s = %s\n", args[0], args[1])
				return nil
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List all config values",
			RunE: func(_ *cobra.Command, _ []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				return json.NewEncoder(os.Stdout).Encode(app.Config.GetAll())
			},
		},
		&cobra.Command{
			Use:   "delete [key]",
			Short: "Delete a config value",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				app, err := openAppFromCwd()
				if err != nil {
					return err
				}
				defer app.Close()

				// Delete by setting to nil, then save.
				if err := app.Config.Set(args[0], nil); err != nil {
					return fmt.Errorf("config delete: %w", err)
				}
				if err := app.Config.Save(); err != nil {
					return fmt.Errorf("config delete save: %w", err)
				}
				_, _ = fmt.Fprintf(os.Stdout, "Deleted %s\n", args[0])
				return nil
			},
		},
	)

	return cmd
}
