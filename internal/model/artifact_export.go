package model

import (
	"context"
	"strings"
)

// ArtifactExportRequest names one guest->host artifact export. Both paths are
// HOST-supplied: the guest names neither its source nor its destination — a
// guest-chosen destination would be a host-filesystem write primitive.
//
// GuestPath is worktree-relative; HostPath is an absolute host destination
// that must not already exist (collisions are refused). Refs: MGIT-73, ADR-011
type ArtifactExportRequest struct {
	GuestPath string `json:"guest_path"`
	HostPath  string `json:"host_path"`
}

// Validate checks the shape of an export request at the boundary, before it
// reaches any backend. It is a cheap first gate; the authoritative containment
// checks (subtree walk, symlink/hardlink escapes, limits, collision) run
// host-side in internal/sandboxd/artifactexport. Refs: MGIT-73, SEC-03
func (r ArtifactExportRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.GuestPath) == "":
		return &ValidationError{Field: "guest_path", Message: "must not be empty"}
	case strings.ContainsRune(r.GuestPath, 0):
		return &ValidationError{Field: "guest_path", Message: "must not contain a NUL byte"}
	case strings.HasPrefix(r.GuestPath, "/"):
		return &ValidationError{Field: "guest_path", Message: "must be worktree-relative, not absolute"}
	case strings.TrimSpace(r.HostPath) == "":
		return &ValidationError{Field: "host_path", Message: "must not be empty"}
	case strings.ContainsRune(r.HostPath, 0):
		return &ValidationError{Field: "host_path", Message: "must not contain a NUL byte"}
	case !strings.HasPrefix(r.HostPath, "/"):
		return &ValidationError{Field: "host_path", Message: "must be an absolute host path"}
	}
	return nil
}

// ArtifactExportResult reports what crossed the boundary. It is what the audit
// record and the caller are built from: which sandbox and task produced the
// artifact, where it landed, how much of it there was, and the content hash of
// the tree. Refs: MGIT-73, FR-17.18
type ArtifactExportResult struct {
	SandboxID    string `json:"sandbox_id"`
	TaskID       string `json:"task_id"`
	GuestPath    string `json:"guest_path"`
	HostPath     string `json:"host_path"`
	ManifestPath string `json:"manifest_path"`
	Files        int    `json:"files"`
	Bytes        int64  `json:"bytes"`
	// TreeHash is the SHA-256 over the exported tree's canonical manifest
	// (ADR-002: SHA-256 is mgit's authoritative hash).
	TreeHash string `json:"tree_hash"`
}

// ArtifactExporter is the OPTIONAL backend capability behind the export verb:
// copy a host-named path out of a running sandbox's worktree to a host-named
// destination (MGIT-73).
//
// It is deliberately not part of SandboxManager. The capability depends on how
// a backend DELIVERS the worktree: libkrun and vzf share a host directory, so
// an export is a host-side read with no guest participation at all, while
// firecracker's launch-time ext4 image would need a guest-mediated stream that
// v1 does not ship. A backend that cannot export says so
// (ErrArtifactExportUnsupported) instead of every backend carrying a method
// most of them would have to fake. Refs: MGIT-73, ADR-011
type ArtifactExporter interface {
	ExportArtifact(ctx context.Context, sandboxID string, req ArtifactExportRequest) (*ArtifactExportResult, error)
}
