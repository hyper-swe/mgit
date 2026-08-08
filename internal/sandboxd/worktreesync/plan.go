// Package worktreesync propagates HOST worktree changes into a running
// sandbox's staged copy, without a live mount and without destroying guest
// work.
//
// WHY A COPY AT ALL. The guest's worktree is a staged copy, and it stays one:
// a live virtiofs share cannot host-side exclude an in-worktree .git/.mgit,
// rebind .mgit to the sandbox-local store, or reject an escaping symlink
// before the guest follows it — so it cannot fail closed (ADR-005, ADR-011,
// the "copy vs. live" note in vzf/hypervisor_darwin.go). Every byte that
// crosses therefore goes through internal/sandboxd/staging's invariants,
// enforced host-side before the guest can act on them.
//
// WHAT THIS PACKAGE DECIDES. Given three views of the tree — what the host
// last DELIVERED, what the host HAS now, and what the GUEST currently has —
// it computes which paths may be updated, which deleted, and which are
// conflicts. That decision is backend-independent; only applying it differs
// (a host-side directory write where the guest's worktree is a shared host
// directory, a guest-mediated push where it is a block image).
//
// Refs: MGIT-71, SEC-03, ADR-005, ADR-011
package worktreesync

import (
	"io/fs"
	"sort"
	"strings"
)

// storePrefix is the guest's private mgit store, which sync never touches.
//
// The guest commits INTO it and land carries those commits back out; a sync
// that overwrote it would destroy exactly the work the airlock exists to
// deliver. It is out of scope rather than a conflict — there is nothing for a
// caller to resolve. Refs: SEC-03, MGIT-71
const storePrefix = ".mgit/"

// storeRoot is the store directory itself (the prefix without its separator).
const storeRoot = ".mgit"

// Entry is one path's identity in a tree: content hash plus mode. The mode
// participates because an executable bit is a real edit.
type Entry struct {
	Hash string
	Mode fs.FileMode
}

// Manifest maps worktree-relative paths to their identity. Directories are not
// listed; they are implied by their files.
type Manifest map[string]Entry

// Reason says why a path could not be synced, so a refusal names something a
// caller can act on rather than "conflict".
type Reason string

const (
	// ReasonModifiedInGuest is set when the guest changed a path the host also
	// changed (or deleted). Landing the guest's work, or --force, resolves it.
	ReasonModifiedInGuest Reason = "modified in the guest since it was delivered"
	// ReasonCreatedInGuest is set when the host added a path the guest
	// independently created. Overwriting would destroy guest work that was
	// never delivered.
	ReasonCreatedInGuest Reason = "created in the guest; the host now has a file of the same name"
)

// Conflict is one path the sync will not touch, and why.
type Conflict struct {
	Path   string
	Reason Reason
}

// Plan is the computed sync: an all-or-nothing set of updates and deletes, or
// the conflicts that block them.
type Plan struct {
	// Update are paths to write from the host's staged tree.
	Update []string
	// Delete are paths to remove from the guest's tree.
	Delete []string
	// Conflicts are paths that block the sync. When the plan was Forced they
	// are still listed — the overwritten paths belong in the audit record.
	Conflicts []Conflict

	forced bool
}

// Empty reports whether there is nothing to do.
func (p Plan) Empty() bool {
	return len(p.Update) == 0 && len(p.Delete) == 0 && len(p.Conflicts) == 0
}

// Blocked reports whether conflicts prevent this plan from being applied. A
// forced plan is never blocked.
func (p Plan) Blocked() bool { return !p.forced && len(p.Conflicts) > 0 }

// Overridden lists the paths a forced plan overwrites despite guest-side
// changes. Empty unless the plan was forced. Every one of these is audited:
// destroying un-landed work silently is the failure this package exists to
// avoid, and --force does not make it acceptable to do so unrecorded.
func (p Plan) Overridden() []string {
	if !p.forced {
		return nil
	}
	out := make([]string, 0, len(p.Conflicts))
	for _, c := range p.Conflicts {
		out = append(out, c.Path)
	}
	return out
}

// Forced returns a copy of the plan with its conflicts converted into updates
// (deletes stay deletes), keeping the conflict list for the audit record.
func (p Plan) Forced() Plan {
	out := Plan{Update: append([]string(nil), p.Update...), Delete: append([]string(nil), p.Delete...),
		Conflicts: p.Conflicts, forced: true}
	for _, c := range p.Conflicts {
		// A conflict is an update unless the host no longer has the path, in
		// which case honoring the host means deleting it.
		out.Update = append(out.Update, c.Path)
	}
	sort.Strings(out.Update)
	return out
}

// Compute classifies every path across the three views and returns the plan.
//
// The classification IS the collision policy (ADR-011):
//
//   - host changed, guest untouched  -> update (the capability being added)
//   - host changed, guest modified   -> CONFLICT (never clobber un-landed work)
//   - host deleted, guest untouched  -> delete
//   - host deleted, guest modified   -> CONFLICT (deletion is as destructive)
//   - host added, absent in guest    -> update
//   - host added, guest has the name -> CONFLICT
//   - guest-created, host never had  -> UNTOUCHED, always
//   - host unchanged                 -> nothing, whatever the guest did
//
// The last two matter most. "Guest-created is untouched" is what keeps
// node_modules and build caches alive across syncs — a naive tree match would
// delete them every round. "Host unchanged is nothing" is what makes an
// automatic pre-exec sync affordable, and it means the guest may edit its copy
// freely without ever colliding with a sync that carries no host change.
// Refs: MGIT-71, ADR-011
func Compute(delivered, host, guest Manifest) Plan {
	var plan Plan
	for path, hostEntry := range host {
		if skipPath(path) {
			continue
		}
		deliveredEntry, wasDelivered := delivered[path]
		if wasDelivered && deliveredEntry == hostEntry {
			continue // the host has not changed this path; nothing to carry
		}
		guestEntry, inGuest := guest[path]
		switch {
		case !wasDelivered && inGuest:
			plan.Conflicts = append(plan.Conflicts, Conflict{path, ReasonCreatedInGuest})
		case wasDelivered && inGuest && guestEntry != deliveredEntry:
			plan.Conflicts = append(plan.Conflicts, Conflict{path, ReasonModifiedInGuest})
		default:
			plan.Update = append(plan.Update, path)
		}
	}
	plan.appendDeletes(delivered, host, guest)
	sort.Strings(plan.Update)
	sort.Strings(plan.Delete)
	sort.Slice(plan.Conflicts, func(i, j int) bool { return plan.Conflicts[i].Path < plan.Conflicts[j].Path })
	return plan
}

// appendDeletes classifies paths the host no longer has. Only paths the host
// once DELIVERED are candidates: anything else is guest-created and is never
// removed.
func (p *Plan) appendDeletes(delivered, host, guest Manifest) {
	for path, deliveredEntry := range delivered {
		if skipPath(path) {
			continue
		}
		if _, stillOnHost := host[path]; stillOnHost {
			continue
		}
		guestEntry, inGuest := guest[path]
		switch {
		case !inGuest:
			continue // already gone in the guest too
		case guestEntry != deliveredEntry:
			p.Conflicts = append(p.Conflicts, Conflict{path, ReasonModifiedInGuest})
		default:
			p.Delete = append(p.Delete, path)
		}
	}
}

// skipPath reports whether a path is out of sync's scope entirely.
func skipPath(path string) bool {
	return path == storeRoot || strings.HasPrefix(path, storePrefix)
}
