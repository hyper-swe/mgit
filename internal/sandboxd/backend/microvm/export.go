package microvm

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/artifactexport"
)

// ExportArtifact copies a HOST-NAMED path out of a running sandbox's worktree
// to a HOST-NAMED destination, so guest-built artifacts (a node_modules tree, a
// build cache) can be reused instead of rebuilt every round.
//
// WHY THERE IS NO CONTROL-PLANE HOP. On the virtiofs backends the guest's
// worktree IS the staged host directory, so an export is a host-side READ of
// that directory with internal/sandboxd/staging's containment checks applied
// outbound. The guest does not participate, is not asked, and cannot observe
// or interpose — which is strictly stronger than a guest-mediated stream would
// be. A backend that delivers the worktree as a launch-time block image has no
// such directory and fails CLOSED with ErrArtifactExportUnsupported rather than
// inventing a weaker path (ADR-011, the same call MGIT-71 made for sync).
//
// The sandbox's sync lock is held for the read, so an export never observes a
// tree that a concurrent worktree sync is halfway through applying.
// Refs: MGIT-73, SEC-03, SEC-10, ADR-011
func (m *Manager) ExportArtifact(_ context.Context, id string, req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("artifact export: %w", err)
	}
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
	staged := stagedTreePath(sb.dir)
	if staged == "" {
		return nil, fmt.Errorf("%w: backend %q delivers the worktree as a launch-time image, "+
			"so there is no host directory to export from", model.ErrArtifactExportUnsupported, m.cfg.Backend)
	}

	sb.syncMu.Lock()
	defer sb.syncMu.Unlock()
	res, err := artifactexport.Export(artifactexport.Request{
		StagedTree: staged,
		GuestPath:  req.GuestPath,
		HostPath:   req.HostPath,
		Now:        m.cfg.Clock().UTC(),
		Provenance: artifactexport.Provenance{
			SandboxID: sb.info.ID, TaskID: sb.info.TaskID,
			Backend: m.cfg.Backend, BaseDigest: sb.info.ImageDigest,
		},
	})
	if err != nil {
		// A refusal is security-relevant in its own right: a reviewer asking
		// "did anything try to leave this sandbox" needs the denials too, not
		// only the exports that succeeded.
		m.cfg.Logger.Warn("artifact export refused",
			"event", "artifact_export_refused", "sandbox_id", sb.info.ID, "task_id", sb.info.TaskID,
			"guest_path", req.GuestPath, "host_path", req.HostPath, "error", err.Error())
		return nil, err
	}
	m.cfg.Logger.Info("artifact exported from the sandbox to the host",
		"event", "artifact_exported", "sandbox_id", sb.info.ID, "task_id", sb.info.TaskID,
		"guest_path", req.GuestPath, "host_path", res.HostPath,
		"files", res.Files, "bytes", res.Bytes, "tree_hash", res.TreeHash)
	return &model.ArtifactExportResult{
		SandboxID: sb.info.ID, TaskID: sb.info.TaskID, GuestPath: req.GuestPath,
		HostPath: res.HostPath, ManifestPath: res.ManifestPath,
		Files: res.Files, Bytes: res.Bytes, TreeHash: res.TreeHash,
	}, nil
}
