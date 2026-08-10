package model

import "context"

// WorktreeSyncOptions selects how one host->guest worktree sync behaves.
//
// The two switches are deliberately separate: Force answers "what happens to
// guest work that collides", DryRun answers "does anything happen at all".
// Combined they answer the question worth asking before forcing — exactly
// which un-landed guest paths would be destroyed. Refs: MGIT-76, FR-17.40, ADR-011
type WorktreeSyncOptions struct {
	// Force overwrites paths the guest changed since delivery. Every
	// overwritten path is reported and audited: destroying un-landed work
	// silently is the failure the collision policy exists to prevent, and
	// asking for it by name does not make it acceptable to do unrecorded.
	Force bool `json:"force,omitempty"`
	// DryRun classifies without touching the guest. It runs the same staging
	// build and the same collision policy a real sync runs — a report derived
	// any other way could claim a sync would succeed where a launch would
	// have refused it.
	DryRun bool `json:"dry_run,omitempty"`
}

// WorktreeSyncConflict is one path a sync will not touch, and why.
//
// Reason is host-authored prose, not a guest-supplied string (SEC-05): the
// classification is computed host-side from three host-held manifests.
// Refs: MGIT-71, MGIT-76
type WorktreeSyncConflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// WorktreeSyncReport is what a sync did — or, when DryRun is set, what it
// WOULD do. One shape serves both so a caller cannot mistake a projection for
// a delivery: every field is qualified by DryRun, which is recorded rather
// than inferred. Refs: MGIT-76, FR-17.40, ADR-011
type WorktreeSyncReport struct {
	Updated    []string               `json:"updated,omitempty"`    // paths delivered into the guest
	Deleted    []string               `json:"deleted,omitempty"`    // paths removed from the guest
	Overridden []string               `json:"overridden,omitempty"` // guest changes destroyed by Force
	Conflicts  []WorktreeSyncConflict `json:"conflicts,omitempty"`  // paths that block (or that Force overwrote)
	// Skipped records a genuine no-op: the host worktree is unchanged since
	// delivery, established by a manifest comparison rather than by re-staging
	// blindly. Reported explicitly so an unchanged worktree says so instead of
	// looking like phantom work.
	Skipped bool `json:"skipped,omitempty"`
	// DryRun records that NOTHING was applied.
	DryRun bool `json:"dry_run,omitempty"`
	// Refused records that a real sync was (or, on a dry run, would be)
	// blocked by conflicts. A refused sync applies nothing at all — not even
	// its unblocked paths.
	Refused bool `json:"refused,omitempty"`
	// Detail is an optional host-authored note explaining an outcome the
	// counts alone do not, such as a sandbox that has not booted yet and so
	// has nothing to propagate into.
	Detail string `json:"detail,omitempty"`
}

// Changed reports whether the guest's tree was altered (or, on a dry run,
// would be).
func (r WorktreeSyncReport) Changed() bool { return len(r.Updated) > 0 || len(r.Deleted) > 0 }

// WorktreeSyncer is the OPTIONAL backend capability of propagating host
// worktree changes into a RUNNING guest.
//
// It is optional because the backends deliver a worktree differently and that
// difference is real, not cosmetic (ADR-011): a virtiofs backend shares the
// staged host directory, so the host can update it in place, while firecracker
// packs an ext4 image at launch that the guest has mounted and the host cannot
// write into. A backend that cannot do this MUST say so — see
// ErrSandboxSyncUnsupported — rather than no-op and report success, because a
// sync that claims to have run is how stale code gets executed.
//
// Implementations MUST route through the same staging path a launch and a
// pre-exec stage use. A sync must never be able to deliver something either of
// those would have refused; that single-path invariant is the whole security
// argument for staging over a live mount (SEC-03).
// Refs: MGIT-71, MGIT-76, FR-17.40, SEC-03, ADR-011
type WorktreeSyncer interface {
	SyncWorktree(ctx context.Context, sandboxID string, opts WorktreeSyncOptions) (*WorktreeSyncReport, error)
}
