package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/hyper-swe/mgit/internal/model"
)

// ExportArtifact copies a host-named path out of a task's running sandbox to a
// host-named destination, and records the crossing in the append-only audit
// trail.
//
// The verb is HOST-INITIATED and both paths are host-supplied: the guest names
// neither, so it can never turn this into a write primitive against the host
// filesystem. The containment checks (traversal, symlink and hardlink escapes,
// collision, size and file-count ceilings) run in the backend's host-side
// export engine before a byte is written; this layer owns task resolution and
// the audit record.
//
// A backend that cannot export (a launch-time-image backend) fails closed with
// ErrArtifactExportUnsupported rather than silently doing something weaker.
// Refs: MGIT-73, FR-17.18, SEC-03, ADR-011
func (s *SandboxService) ExportArtifact(ctx context.Context, taskID string,
	req model.ArtifactExportRequest) (*model.ArtifactExportResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox export: %w", err)
	}
	exporter, ok := s.manager.(model.ArtifactExporter)
	if !ok {
		return nil, fmt.Errorf("%w: backend has no export path", model.ErrArtifactExportUnsupported)
	}
	info, err := s.runningSandbox(ctx, taskID)
	if err != nil {
		return nil, err
	}
	res, err := exporter.ExportArtifact(ctx, info.ID, req)
	if err != nil {
		return nil, err
	}
	if auditErr := s.auditExport(ctx, res); auditErr != nil {
		return nil, auditErr
	}
	return res, nil
}

// runningSandbox resolves a task's sandbox and requires it to be booted: there
// is no worktree to read out of a sandbox that has never run.
//
// An in-flight boot of that task is waited out (awaitBootLocked) rather than
// refused: the sandbox is about to be running, and refusing it here would make
// an export fail for timing rather than for state. Refs: MGIT-73, MGIT-122
func (s *SandboxService) runningSandbox(ctx context.Context, taskID string) (model.SandboxInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, err := s.awaitBootLocked(ctx, taskID)
	if err != nil {
		return model.SandboxInfo{}, err
	}
	if !reg.booted {
		return model.SandboxInfo{}, fmt.Errorf("%w: task %q has a registered sandbox that has not booted; "+
			"run something in it first", model.ErrSandboxBackendUnavailable, taskID)
	}
	return reg.info, nil
}

// auditExport records the crossing. A file that reaches the host without a
// record defeats the audit trail, so an un-auditable export is UNDONE: the
// artifact and its sidecar are removed and the caller is told both facts. Both
// paths were named by the host and were created by this export, so removing
// them cannot destroy pre-existing host state (a collision would have refused
// the export before anything was written). Refs: MGIT-73, FR-17.18
func (s *SandboxService) auditExport(ctx context.Context, res *model.ArtifactExportResult) error {
	detail, err := json.Marshal(map[string]any{
		"guest_path": res.GuestPath, "host_path": res.HostPath,
		"manifest_path": res.ManifestPath, "files": res.Files,
		"bytes": res.Bytes, "tree_hash": res.TreeHash,
	})
	if err != nil {
		return fmt.Errorf("sandbox export: encode audit detail: %w", err)
	}
	auditErr := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: res.SandboxID, TaskID: res.TaskID,
		EventType: model.EventArtifactExported, Detail: string(detail),
	})
	if auditErr == nil {
		return nil
	}
	return errors.Join(
		fmt.Errorf("sandbox export: audit: %w (the export was undone: %s)", auditErr, res.HostPath),
		os.RemoveAll(res.HostPath), os.RemoveAll(res.ManifestPath),
	)
}
