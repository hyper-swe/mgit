package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit-dev/internal/service"
	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
	"github.com/hyper-swe/mgit-dev/internal/store/index"
)

// initCmd implements mgit init. Refs: FR-8.1, MGIT-4.1.1
func initCmd() *cobra.Command {
	var path string
	var linkMtix bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new mgit repository",
		RunE: func(_ *cobra.Command, _ []string) error {
			if path == "" {
				var err error
				path, err = os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
			}

			clock := func() time.Time { return time.Now().UTC() }
			repo, err := gitstore.Init(path, clock)
			if err != nil {
				return fmt.Errorf("init: %w", err)
			}
			defer func() { _ = repo.Close() }()

			// Create SQLite index
			dbPath := filepath.Join(path, ".mgit", "index.db")
			idx, err := index.New(dbPath, clock)
			if err != nil {
				return fmt.Errorf("init index: %w", err)
			}
			_ = idx.Close()

			// Create default config
			configPath := filepath.Join(path, ".mgit", "config.json")
			cfgSvc, err := service.NewConfigService(configPath)
			if err != nil {
				return fmt.Errorf("init config: %w", err)
			}
			if err := cfgSvc.Save(); err != nil {
				return fmt.Errorf("save config: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stdout, "Initialized mgit repository at %s\n", filepath.Join(path, ".mgit"))

			// --link-mtix: check for mtix project directory and print linking note.
			if linkMtix {
				mtixPaths := []string{
					filepath.Join(path, ".mtix"),
					filepath.Join(filepath.Dir(path), "mtix"),
				}
				for _, mp := range mtixPaths {
					if info, err := os.Stat(mp); err == nil && info.IsDir() {
						_, _ = fmt.Fprintf(os.Stdout, "Found mtix project at %s — link available\n", mp)
						break
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Repository path (default: current directory)")
	cmd.Flags().BoolVar(&linkMtix, "link-mtix", false, "Check for mtix project directory and print linking note")
	return cmd
}
