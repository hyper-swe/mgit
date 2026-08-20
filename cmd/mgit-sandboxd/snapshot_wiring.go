package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// snapshotWatcher is the production WorktreeWatcher: on each pass it asks the
// service which sandboxes are supervised, and observes each one's bound
// worktree for quiescence.
//
// It opens the task's worktree as a LINKED repository — root at the worktree
// so the scan sees the agent's files, objects in the shared .mgit store so
// snapshots dedup against everything already there. A per-pass open keeps no
// handle alive between ticks, which matters because the CLI writes to the same
// store between them. Refs: MGIT-110, R-H234, ADR-007
type snapshotWatcher struct {
	lister   func(ctx context.Context) ([]model.SandboxInfo, error)
	repoRoot string
	clock    func() time.Time
	logger   *slog.Logger
	// perTask caches one SnapshotService per task, so quiescence state (the
	// previous fingerprint) survives between passes. Without it every pass
	// would look like a first observation and nothing would ever settle.
	perTask map[string]*service.SnapshotService
}

// newSnapshotWatcher builds the production watcher. Refs: MGIT-110
func newSnapshotWatcher(
	lister func(ctx context.Context) ([]model.SandboxInfo, error),
	repoRoot string, clock func() time.Time, logger *slog.Logger,
) *snapshotWatcher {
	return &snapshotWatcher{
		lister: lister, repoRoot: repoRoot, clock: clock, logger: logger,
		perTask: map[string]*service.SnapshotService{},
	}
}

// Observe runs one pass over every supervised task. A failure on one task is
// logged and the pass continues: one unreadable worktree must not cost every
// other task its recovery point. Refs: MGIT-110
func (w *snapshotWatcher) Observe(ctx context.Context) error {
	sandboxes, err := w.lister(ctx)
	if err != nil {
		return fmt.Errorf("snapshot watch: list sandboxes: %w", err)
	}
	var errs []error
	for _, sb := range sandboxes {
		if sb.WorktreePath == "" || sb.TaskID == "" {
			continue
		}
		if err := w.observeTask(ctx, sb); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// observeTask opens the task's worktree and runs one quiescence observation.
func (w *snapshotWatcher) observeTask(ctx context.Context, sb model.SandboxInfo) error {
	repo, err := gitstore.OpenLinked(sb.WorktreePath,
		filepath.Join(w.repoRoot, ".mgit"), taskBranch(sb.TaskID), w.clock)
	if err != nil {
		return fmt.Errorf("snapshot watch %s: open worktree: %w", sb.TaskID, err)
	}
	defer func() { _ = repo.Close() }()

	svc, ok := w.perTask[sb.TaskID]
	if !ok {
		svc = service.NewSnapshotService(gitstore.NewSnapshotStore(repo), w.clock, 0)
		w.perTask[sb.TaskID] = svc
	} else {
		svc.Rebind(gitstore.NewSnapshotStore(repo))
	}
	snap, err := svc.Observe(ctx, sb.TaskID)
	if err != nil {
		return err
	}
	if snap != nil {
		w.logger.Info("worktree snapshot captured", "event", "snapshot",
			"task_id", snap.TaskID, "snapshot_id", snap.ID, "files", snap.FileCount)
	}
	return nil
}

// taskBranch is the branch convention a task worktree is bound to.
func taskBranch(taskID string) string { return "task/" + taskID }

// buildSnapshotWatcher wires the passive watcher.
//
// The repo root is recovered from the conventional <repo>/.mgit/sandbox host
// root when it was not passed explicitly — the same fallback the land path
// uses. Without it a daemon started without --repo-root would take no
// snapshots at all, and it would do so silently, which is the failure mode
// this whole feature exists to remove: an absent guarantee that looks
// identical to a working one.
//
// When it genuinely cannot be wired, that is LOGGED rather than left to
// silence, so "no snapshots exist" is never mistaken for "nothing changed".
// Refs: MGIT-110, R-H234
func buildSnapshotWatcher(svc sandboxd.SandboxDispatcher, repoRoot, hostRoot string,
	clock func() time.Time, logger *slog.Logger) sandboxd.WorktreeWatcher {
	if svc == nil {
		logger.Warn("passive worktree snapshots are OFF: no sandbox service is wired, "+
			"so this daemon cannot enumerate task worktrees; an interrupted agent will "+
			"be recoverable only from what it committed itself",
			"event", "snapshot_unavailable")
		return nil
	}
	if repoRoot == "" {
		repoRoot = filepath.Dir(filepath.Dir(hostRoot))
	}
	if repoRoot == "" || repoRoot == "." {
		logger.Warn("passive worktree snapshots are OFF: the repository root could not be "+
			"determined from --repo-root or --host-root",
			"event", "snapshot_unavailable", "host_root", hostRoot)
		return nil
	}
	logger.Info("passive worktree snapshots are ON", "event", "snapshot_enabled", "repo_root", repoRoot)
	return newSnapshotWatcher(svc.List, repoRoot, clock, logger)
}
