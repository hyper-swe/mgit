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
	// Truncated records that the path lists above are SHORTER than what was
	// found, and the *Total counts say by how much.
	//
	// It exists so a shortened list can never be mistaken for a complete one.
	// A caller acting on "these 500 paths differ" when 40,000 do has been
	// misled by its own tooling — which is worse than the crash this bounding
	// replaced, because a crash is not believed and a wrong answer is.
	// An UNMARKED report therefore means exactly one thing: nothing was
	// dropped. Refs: MGIT-160
	Truncated bool `json:"truncated,omitempty"`
	// The full counts, always populated whether or not the lists were
	// shortened, so a reader never has to infer a total from a list length.
	UpdatedTotal    int `json:"updated_total,omitempty"`
	DeletedTotal    int `json:"deleted_total,omitempty"`
	OverriddenTotal int `json:"overridden_total,omitempty"`
	ConflictsTotal  int `json:"conflicts_total,omitempty"`
}

// SyncReportPathLimit is how many paths of each kind a report carries.
//
// A classification that can only answer for small trees is not a
// classification: a worktree holding a host-side node_modules enumerates tens
// of thousands of paths, and the whole answer was silently dropped for
// exceeding the control-response limit (MGIT-160). The limit is chosen so that
// every list, at realistic path lengths, fits well inside that budget with the
// rest of the report — not so that a typical case squeaks under it.
const SyncReportPathLimit = 500

// Bound returns a copy of the report whose path lists carry at most limit
// entries each, with the full counts preserved and Truncated set when anything
// was dropped.
//
// Bounding happens at the MODEL, not at one call site, so every producer and
// transport of a report inherits the same guarantee — and a new caller cannot
// forget it. Refs: MGIT-160
func (r WorktreeSyncReport) Bound(limit int) WorktreeSyncReport {
	out := r
	out.UpdatedTotal = len(r.Updated)
	out.DeletedTotal = len(r.Deleted)
	out.OverriddenTotal = len(r.Overridden)
	out.ConflictsTotal = len(r.Conflicts)
	if limit <= 0 {
		return out
	}
	out.Updated, out.Truncated = capPaths(r.Updated, limit, out.Truncated)
	out.Deleted, out.Truncated = capPaths(r.Deleted, limit, out.Truncated)
	out.Overridden, out.Truncated = capPaths(r.Overridden, limit, out.Truncated)
	if len(r.Conflicts) > limit {
		out.Conflicts, out.Truncated = r.Conflicts[:limit], true
	}
	return out
}

// capPaths shortens a list to limit, reporting whether anything was dropped
// (never clearing a truncation another list already recorded).
func capPaths(in []string, limit int, already bool) ([]string, bool) {
	if len(in) <= limit {
		return in, already
	}
	return in[:limit], true
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
