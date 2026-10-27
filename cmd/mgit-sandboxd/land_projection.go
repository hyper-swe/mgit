package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/hyper-swe/mgit/internal/store/lock"
)

// landProjection publishes verified land objects into the host-recorded native
// worktree, not the daemon's repository-root working tree. It is only a brancher:
// imports and attestations remain behind the existing orchestrator. Refs: MGIT-317
// The shared process lock serializes projection with native CLI operations.
type landProjection struct {
	repoRoot string
	resolver service.SandboxResolver
	idx      *index.Store
	clock    func() time.Time
}

func (p *landProjection) guard(fn func() error) error {
	store := filepath.Join(p.repoRoot, ".mgit")
	return lock.NewGuarder(store, service.LockTimeoutFromConfig(filepath.Join(store, "config.json"))).Guard(fn)
}

// target opens a task-scoped view only after host registry and linked marker
// agree. No path or branch supplied by a guest chooses a host write target.
func (p *landProjection) target(ctx context.Context, task string) (*gitstore.Repository, error) {
	info, err := p.resolver.Status(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("resolve native worktree: %w", err)
	}
	if info == nil || info.WorktreePath == "" || !filepath.IsAbs(info.WorktreePath) {
		return nil, fmt.Errorf("native worktree binding is missing")
	}
	path, err := filepath.EvalSymlinks(info.WorktreePath)
	if err != nil {
		return nil, fmt.Errorf("native worktree unavailable: %w", err)
	}
	root, err := filepath.EvalSymlinks(p.repoRoot)
	if err != nil {
		return nil, err
	}
	branch := model.TaskBranchName(task)
	wt, err := p.idx.GetWorktreeByTask(ctx, task)
	if err != nil && (!errors.Is(err, model.ErrWorktreeNotFound) || path != root) {
		return nil, fmt.Errorf("native worktree registration: %w", err)
	}
	if wt != nil {
		registeredPath := wt.Path
		if !filepath.IsAbs(registeredPath) {
			// The host registry can contain a relative `mgit work wt` path.
			// Anchor it to the trusted repository, never the daemon's cwd.
			registeredPath = filepath.Join(root, registeredPath)
		}
		registered, e := filepath.EvalSymlinks(registeredPath)
		if e != nil || registered != path || wt.Branch != branch {
			return nil, fmt.Errorf("native worktree registration does not match sandbox binding")
		}
	}
	store := filepath.Join(root, ".mgit")
	if path != root {
		marker, linked, e := gitstore.ReadWorktreeMarker(path)
		if e != nil {
			return nil, e
		}
		if !linked {
			return nil, fmt.Errorf("native worktree marker is missing")
		}
		shared, e := filepath.EvalSymlinks(marker.Store)
		expected, e2 := filepath.EvalSymlinks(store)
		if e != nil || e2 != nil || shared != expected || marker.Task != task || marker.Branch != branch {
			return nil, fmt.Errorf("native worktree marker does not match task, branch, and shared store")
		}
	}
	return gitstore.OpenLinked(path, store, branch, p.clock)
}

// FastForward publishes only verified objects through a task-scoped native view.
func (p *landProjection) FastForward(ctx context.Context, task, tip string) error {
	return p.guard(func() error {
		repo, err := p.target(ctx, task)
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		return p.publish(ctx, repo, task, tip)
	})
}

func (p *landProjection) publish(ctx context.Context, repo *gitstore.Repository, task, tip string) error {
	err := gitstore.NewMergeStore(repo).FastForwardDelivered(ctx, model.TaskBranchName(task), tip)
	var divergence *gitstore.ProjectionDivergenceError
	if errors.As(err, &divergence) {
		// The importer appends verified commits before projection. Confirm the
		// durable record before claiming retention in a diagnostic.
		commits, readErr := p.idx.GetTaskCommits(ctx, task)
		if readErr != nil {
			return fmt.Errorf("%w; cannot confirm ledger retention: %w; no automatic recovery yet: MGIT-319", err, readErr)
		}
		for _, commit := range commits {
			if commit.CommitHash == tip {
				return fmt.Errorf("%w; guest commit %.8s is retained in task ledger; no automatic recovery yet: MGIT-319", err, tip)
			}
		}
		return fmt.Errorf("%w; no automatic recovery yet: MGIT-319", err)
	}
	var conflict *gitstore.ProjectionConflictError
	if errors.As(err, &conflict) {
		path := projectionShellQuote("./" + conflict.Path)
		remedy := fmt.Sprintf("move %s to a backup outside the worktree first", path)
		if conflict.TargetExists {
			if conflict.TargetMode == model.FileModeSymlink {
				remedy += fmt.Sprintf("; run ln -s -- %s %s", projectionShellQuote(conflict.LinkTarget), path)
			} else {
				remedy += fmt.Sprintf("; run mgit restore %s --commit %s", path, tip)
				if conflict.TargetMode == model.FileModeExecutable {
					remedy += fmt.Sprintf("; run chmod 700 %s", path)
				}
			}
		}
		return fmt.Errorf("%w; %s; run mgit restore --staged %s, then mgit sandbox land --task-id %s", err, remedy, path, task)
	}
	return err
}

// Check completes an already-attested pending projection using the same safe
// publisher. A durable ledger append is not proof of delivery, nor a reason to
// wedge retries. A newer native tip is preserved, never rewound.
func (p *landProjection) Check(ctx context.Context, task string) error {
	return p.guard(func() error {
		repo, err := p.target(ctx, task)
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		commits, err := p.idx.GetTaskCommits(ctx, task)
		if err != nil {
			return err
		}
		if len(commits) == 0 {
			return nil
		}
		expected := commits[len(commits)-1].CommitHash
		tip, err := repo.Head()
		if err != nil {
			return err
		}
		if tip != expected {
			ahead, e := gitstore.NewMergeStore(repo).IsAncestor(ctx, expected, tip)
			if e != nil {
				return e
			}
			if ahead {
				expected = tip
			}
		}
		return p.publish(ctx, repo, task, expected)
	})
}

// Paths and link text originate in commit trees, not in a shell grammar.
func projectionShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
