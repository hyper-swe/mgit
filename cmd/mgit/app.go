// Package main is the entry point for the mgit CLI.
// Refs: FR-8, MGIT-4.1.1
package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/astutic/mgit/internal/service"
	gitstore "github.com/astutic/mgit/internal/store/git"
	"github.com/astutic/mgit/internal/store/index"
)

// App holds all initialized services for the CLI.
// Created by OpenApp after mgit init has been run.
// Refs: FR-8
type App struct {
	Repo     *gitstore.Repository
	Index    *index.Store
	Commit   *service.CommitService
	Squash   *service.SquashService
	Rollback *service.RollbackService
	Branch   *service.BranchService
	Verify   *service.VerifyService
	Audit    *service.AuditService
	Config   *service.ConfigService
	Diff     *service.DiffService
}

// OpenApp opens an existing mgit repository and initializes all services.
// Returns an error if no .mgit/ directory exists.
func OpenApp(path string) (*App, error) {
	clock := func() time.Time { return time.Now().UTC() }

	repo, err := gitstore.Open(path, clock)
	if err != nil {
		return nil, fmt.Errorf("open repository: %w", err)
	}

	dbPath := filepath.Join(path, ".mgit", "index.db")
	idx, err := index.New(dbPath, clock)
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("open index: %w", err)
	}

	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)
	ds := gitstore.NewDiffStore(repo)
	auditPath := filepath.Join(path, ".mgit", "audit.log")
	configPath := filepath.Join(path, ".mgit", "config.json")

	cfgSvc, err := service.NewConfigService(configPath)
	if err != nil {
		_ = idx.Close()
		_ = repo.Close()
		return nil, fmt.Errorf("load config: %w", err)
	}

	return &App{
		Repo:     repo,
		Index:    idx,
		Commit:   service.NewCommitService(repo, cs, idx),
		Squash:   service.NewSquashService(repo, cs, idx),
		Rollback: service.NewRollbackService(repo, cs, idx),
		Branch:   service.NewBranchService(repo, bs, idx),
		Verify:   service.NewVerifyService(cs, idx),
		Audit:    service.NewAuditService(auditPath, clock),
		Config:   cfgSvc,
		Diff:     service.NewDiffService(ds, cs, idx),
	}, nil
}

// Close shuts down all stores.
func (a *App) Close() {
	if a.Index != nil {
		_ = a.Index.Close()
	}
	if a.Repo != nil {
		_ = a.Repo.Close()
	}
}
