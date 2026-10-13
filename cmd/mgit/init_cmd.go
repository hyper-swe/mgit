package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// initCmd implements mgit init. Refs: FR-8.1, MGIT-4.1.1
func initCmd() *cobra.Command {
	var path string
	var linkMtix bool
	var noAgentFiles bool

	cmd := &cobra.Command{
		Use:   "init",
		Args:  refuseArgs("init", "Run `mgit init` inside the directory to initialize."),
		Short: "Initialize a new mgit repository",
		RunE: func(_ *cobra.Command, _ []string) error {
			if path == "" {
				var err error
				path, err = os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
			}

			// The agent instructions are mgit's own file plus pointer lines. They
			// are written BEFORE the base is recorded, so they are part of the
			// base and the first task's patch does not carry them. A failure
			// here must not stop the repository from being initialized. A
			// directory that is already initialized is left to Init to report.
			var entryOut bytes.Buffer
			var entryErr error
			var undoEntry func()
			madeDir := false
			if _, statErr := os.Stat(filepath.Join(path, ".mgit")); !noAgentFiles && os.IsNotExist(statErr) {
				// init --path may name a directory that does not exist yet;
				// Init used to create it, and the files now come first.
				if _, dirErr := os.Stat(path); os.IsNotExist(dirErr) {
					if err := os.MkdirAll(path, 0o750); err != nil {
						return fmt.Errorf("create %s: %w", path, err)
					}
					madeDir = true
				}
				undoEntry = snapshotAgentFiles(path)
				entryErr = writeAgentEntry(path, &entryOut)
			}

			clock := func() time.Time { return time.Now().UTC() }
			repo, err := gitstore.Init(path, clock)
			if err != nil {
				// take back what was written ahead of the base: files init
				// created are removed, a file the project had is restored, and
				// a directory init made is removed again if nothing is in it
				if undoEntry != nil {
					undoEntry()
				}
				if madeDir {
					_ = os.Remove(path)
				}
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

			_, _ = os.Stdout.Write(entryOut.Bytes())
			if entryErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "warning: could not write the agent instructions (%v)\n", entryErr)
			}

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
	cmd.Flags().BoolVar(&noAgentFiles, "no-agent-files", false,
		"Do not write MGIT.md or the CLAUDE.md/AGENTS.md pointer lines")
	cmd.Flags().BoolVar(&linkMtix, "link-mtix", false, "Check for mtix project directory and print linking note")
	return cmd
}
