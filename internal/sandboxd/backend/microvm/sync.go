package microvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// stagedTreeName is the per-VM staged worktree directory the virtiofs backends
// share into the guest. It is a shared convention rather than a per-backend
// secret: libkrun and vzf both stage here, which is what lets the manager
// propagate host changes without knowing which of them is running.
//
// A backend that delivers the worktree as a block image (firecracker) has no
// such directory, and stagedTreePath reports that by returning "".
// Refs: MGIT-71, ADR-005, ADR-011
const stagedTreeName = "worktree-staging"

// stagedTreePath returns the staged worktree directory for a sandbox, or ""
// when this backend does not deliver via a shared host directory.
func stagedTreePath(stateDir string) string {
	p := filepath.Join(stateDir, stagedTreeName)
	if info, err := os.Stat(p); err != nil || !info.IsDir() {
		return ""
	}
	return p
}

// HasStagedTree reports whether a sandbox state dir carries a staged worktree
// — the directory the guest actually mounts.
//
// It exists so real-VM tests outside this package can assert against the SAME
// directory the backend delivers into, instead of hardcoding the name. A test
// that hardcodes it and drifts would write to a path the guest never mounts,
// and every host-side assertion would still pass. Refs: MGIT-76
func HasStagedTree(stateDir string) bool { return stagedTreePath(stateDir) != "" }

// ErrSyncUnsupported is returned when a backend cannot propagate host changes
// into a running guest.
//
// It IS model.ErrSandboxSyncUnsupported so every layer above can classify the
// refusal rather than parse it. This is a real limitation, reported rather
// than papered over: firecracker delivers the worktree as an ext4 image built
// at launch and mounted by the guest, so the host cannot write into it without
// corrupting it. Such a sandbox keeps launch-time-copy semantics.
// Refs: MGIT-71, MGIT-76, ADR-011
var ErrSyncUnsupported = model.ErrSandboxSyncUnsupported

// SyncWorktree propagates host worktree changes into a running sandbox, or —
// with DryRun — reports what it would do without touching the guest.
//
// It is the explicit verb (`mgit sandbox sync`); Exec calls the SAME path
// automatically so an agent loop does not have to remember to. There is
// deliberately no second staging path: a sync must never be able to deliver
// what a launch or a pre-exec stage would have refused, and that single-path
// invariant is the whole security argument for staging over a live mount.
//
// A conflict returns BOTH the classification and the refusal error, so a
// caller can name the diverged paths without re-deriving them.
// Refs: MGIT-71, MGIT-76, FR-17.40, SEC-03, ADR-011
func (m *Manager) SyncWorktree(ctx context.Context, id string, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State != model.StateRunning {
		return nil, fmt.Errorf("%w: sandbox %q is %s, not running",
			model.ErrSandboxBackendUnavailable, id, sb.info.State)
	}
	return m.syncLocked(ctx, sb, opts)
}

// syncLocked runs one sync under the sandbox's sync lock, which is also held
// across exec — so a command never observes a partially applied tree. A dry
// run takes the same lock: a classification read while another sync was
// applying would describe a tree that never existed.
func (m *Manager) syncLocked(ctx context.Context, sb *sandbox, opts model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	staged := stagedTreePath(sb.dir)
	if staged == "" {
		return nil, fmt.Errorf("%w: backend %q delivers the worktree as a "+
			"launch-time image; re-launch to pick up host changes",
			ErrSyncUnsupported, m.cfg.Backend)
	}
	if sb.info.WorktreePath == "" || sb.privateStore == "" {
		// A sandbox launched without a worktree has nothing to propagate.
		return &model.WorktreeSyncReport{Skipped: true, DryRun: opts.DryRun,
			Detail: "this sandbox was launched without a worktree, so there is nothing to propagate"}, nil
	}

	sb.syncMu.Lock()
	defer sb.syncMu.Unlock()
	res, err := worktreesync.Sync(worktreesync.Request{
		WorktreePath:     sb.info.WorktreePath,
		PrivateStorePath: sb.privateStore,
		StateDir:         sb.dir,
		GuestTree:        staged,
		Force:            opts.Force,
		DryRun:           opts.DryRun,
	})
	if err != nil {
		return refusalReport(err), err
	}
	report := toReport(res)
	// The host read-back above proves the bytes landed in the host's
	// directory. Whether the guest reads them is a separate fact — its kernel
	// keeps its own view for a window after its last access — so nothing is
	// reported delivered until the guest itself confirms it. Refs: MGIT-192
	note, err := m.settleGuest(ctx, sb, res)
	if err != nil {
		m.cfg.Logger.Warn("worktree sync refused: the guest did not settle",
			"event", "sync_unsettled", "sandbox_id", sb.info.ID, "task_id", sb.info.TaskID, "error", err.Error())
		return nil, err
	}
	report.Detail = note
	m.auditSync(sb, report)
	return report, nil
}

// refusalReport turns a conflict refusal into the classification that caused
// it, so a caller does not have to re-run a dry run to learn which paths
// diverged. Any other failure carries no classification. Refs: MGIT-76
func refusalReport(err error) *model.WorktreeSyncReport {
	var conflict *worktreesync.ConflictError
	if !errors.As(err, &conflict) {
		return nil
	}
	return &model.WorktreeSyncReport{Refused: true, Conflicts: toConflicts(conflict.Conflicts)}
}

// toReport converts the backend-independent sync result into the model shape
// the service and every caller above it speak.
func toReport(res worktreesync.Result) *model.WorktreeSyncReport {
	return &model.WorktreeSyncReport{
		Updated:    worktreesync.SortedPaths(res.Updated),
		Deleted:    worktreesync.SortedPaths(res.Deleted),
		Overridden: worktreesync.SortedPaths(res.Overridden),
		Conflicts:  toConflicts(res.Conflicts),
		Skipped:    res.Skipped,
		DryRun:     res.DryRun,
		Refused:    res.Blocked,
	}
}

// toConflicts converts the classification's conflicts, whose reasons are
// host-authored prose (SEC-05).
func toConflicts(in []worktreesync.Conflict) []model.WorktreeSyncConflict {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.WorktreeSyncConflict, 0, len(in))
	for _, c := range in {
		out = append(out, model.WorktreeSyncConflict{Path: c.Path, Reason: string(c.Reason)})
	}
	return out
}

// auditSync records a propagation that CHANGED what the guest runs.
//
// A dry run is never recorded as a sync: it delivered nothing, and an audit
// trail that cannot distinguish a query from a delivery is worse than none —
// a reviewer reconstructing "what code did this sandbox execute" would be
// reading events that never happened. Refs: FR-17.18, MGIT-76
func (m *Manager) auditSync(sb *sandbox, report *model.WorktreeSyncReport) {
	if report.DryRun || (!report.Changed() && len(report.Overridden) == 0) {
		return
	}
	m.cfg.Logger.Info("worktree synced into the running sandbox",
		"event", "worktree_synced", "sandbox_id", sb.info.ID, "task_id", sb.info.TaskID,
		"updated", report.Updated, "deleted", report.Deleted, "overridden", report.Overridden)
}

// syncBeforeExec propagates host changes ahead of a command, so a sandboxed
// agent loop tests the code the host actually has.
//
// A backend that cannot sync keeps its existing semantics rather than failing
// every exec. A CONFLICT, however, fails the exec loudly: running a command
// against knowingly stale code is the defect this exists to prevent, so
// "blocked, with the conflicting paths named" beats "ran, against the wrong
// tree". Refs: MGIT-71, ADR-011
func (m *Manager) syncBeforeExec(ctx context.Context, sb *sandbox) error {
	if stagedTreePath(sb.dir) == "" {
		return nil // launch-time-copy backend; unchanged behavior
	}
	_, err := m.syncLocked(ctx, sb, model.WorktreeSyncOptions{})
	switch {
	case err == nil, errors.Is(err, ErrSyncUnsupported):
		return nil
	case errors.Is(err, worktreesync.ErrConflict):
		return fmt.Errorf("exec refused: the host worktree changed but %w", err)
	default:
		return fmt.Errorf("exec refused: could not sync the host worktree: %w", err)
	}
}
