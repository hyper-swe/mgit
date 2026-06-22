// Package main is the entry point for the mgit CLI.
// Refs: FR-8, MGIT-4.1.1
package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
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
	Restore  *service.RestoreService
	Checkout *service.CheckoutService
	Merge    *service.MergeService
	GC       *service.GCService
	Bundle   *service.BundleService

	fileLock *lock.FileLock
}

// OpenApp opens an existing mgit repository and initializes all services.
// Acquires a process-level file lock to serialize concurrent CLI access.
// Returns an error if no .mgit/ directory exists or if another mgit process
// holds the lock for longer than the timeout.
func OpenApp(path string) (*App, error) {
	clock := func() time.Time { return time.Now().UTC() }

	mgitDir := filepath.Join(path, ".mgit")

	// Acquire process-level lock before opening any stores.
	// This prevents races between concurrent CLI processes on the same repo.
	fileLock, err := lock.Acquire(mgitDir, lock.DefaultTimeout)
	if err != nil {
		return nil, err
	}

	repo, err := gitstore.Open(path, clock)
	if err != nil {
		_ = fileLock.Release()
		return nil, fmt.Errorf("open repository: %w", err)
	}

	dbPath := filepath.Join(mgitDir, "index.db")
	idx, err := index.New(dbPath, clock)
	if err != nil {
		_ = repo.Close()
		_ = fileLock.Release()
		return nil, fmt.Errorf("open index: %w", err)
	}

	cs := gitstore.NewCommitStore(repo)
	bs := gitstore.NewBranchStore(repo)
	ds := gitstore.NewDiffStore(repo)
	ws := gitstore.NewWorktreeStore(repo)
	ms := gitstore.NewMergeStore(repo)
	gcs := gitstore.NewGCStore(repo)
	auditPath := filepath.Join(mgitDir, "audit.log")
	configPath := filepath.Join(mgitDir, "config.json")

	cfgSvc, err := service.NewConfigService(configPath)
	if err != nil {
		_ = idx.Close()
		_ = repo.Close()
		_ = fileLock.Release()
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Audit trail is shared so commit/squash/rollback can record operations
	// surfaced by `mgit audit` (MGIT-20).
	audit := service.NewAuditService(auditPath, clock)

	return &App{
		Repo:     repo,
		Index:    idx,
		Commit:   service.NewCommitService(repo, cs, idx).WithAudit(audit),
		Squash:   service.NewSquashService(repo, cs, idx).WithAudit(audit),
		Rollback: service.NewRollbackService(repo, cs, idx).WithAudit(audit),
		Branch:   service.NewBranchService(repo, bs, idx),
		Verify:   service.NewVerifyService(cs, idx),
		Audit:    audit,
		Config:   cfgSvc,
		Diff:     service.NewDiffService(ds, cs, idx),
		Restore:  service.NewRestoreService(cs, path),
		Checkout: service.NewCheckoutService(bs, ws),
		Merge:    service.NewMergeService(repo, bs, ms, cs),
		GC:       service.NewGCService(gcs),
		Bundle:   service.NewBundleService(idx, clock),
		fileLock: fileLock,
	}, nil
}

// Close shuts down all stores and releases the process-level lock.
func (a *App) Close() {
	if a.Index != nil {
		_ = a.Index.Close()
	}
	if a.Repo != nil {
		_ = a.Repo.Close()
	}
	if a.fileLock != nil {
		_ = a.fileLock.Release()
	}
}
