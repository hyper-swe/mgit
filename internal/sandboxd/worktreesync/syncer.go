package worktreesync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// manifestName is the per-sandbox record of what the host last DELIVERED,
// kept in the sandbox state dir so teardown's single RemoveAll reclaims it
// (FR-17.19). It is host-only state: the guest never reads or writes it, and a
// guest that misreports its own tree cannot alter it.
const manifestName = "sync-manifest.json"

// stagedSubdir is where a candidate tree is built before any comparison. It
// lives beside the live staged tree, inside the sandbox state dir.
const stagedSubdir = "worktree-sync-candidate"

// Result reports what a sync did — or, for a dry run, what it WOULD do — for
// the caller and for the audit record.
type Result struct {
	Updated    []string // paths delivered into the guest
	Deleted    []string // paths removed from the guest
	Overridden []string // guest-modified paths overwritten by --force
	Skipped    bool     // the host worktree was unchanged; nothing was done
	// DryRun records that nothing was applied: every other field describes
	// what a real sync would have done. It is set from the request rather
	// than inferred, so a caller reading a Result can never mistake a
	// classification for a delivery. Refs: MGIT-76
	DryRun bool
	// Conflicts are the paths that block (or, under Force, that were
	// overwritten). A real sync reports them through ConflictError and
	// applies nothing; a dry run reports them HERE, which is the whole point
	// of the query — today the only way to discover a conflict is to attempt
	// work and be refused. Refs: MGIT-76
	Conflicts []Conflict
	// Blocked reports that a real, unforced sync would be refused. Only
	// meaningful on a dry run: a blocked real sync returns ConflictError
	// instead of a Result. Refs: MGIT-76
	Blocked bool
}

// Changed reports whether the sync altered the guest's tree (or, on a dry run,
// whether it would).
func (r Result) Changed() bool { return len(r.Updated) > 0 || len(r.Deleted) > 0 }

// ConflictError is the refusal a blocked sync returns. It names every
// conflicting path and its reason, because "conflict" alone gives a caller
// nothing to act on. Refs: MGIT-71
type ConflictError struct{ Conflicts []Conflict }

func (e *ConflictError) Error() string {
	paths := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		paths = append(paths, fmt.Sprintf("%s (%s)", c.Path, c.Reason))
	}
	return fmt.Sprintf("%v: %d path(s) changed in the guest since delivery: %s; "+
		"land the guest's work or re-run with --force to overwrite",
		ErrConflict, len(e.Conflicts), strings.Join(paths, ", "))
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Request is one sandbox's sync inputs. They travel together because they are
// all per-sandbox launch facts the caller already holds.
type Request struct {
	WorktreePath     string // the user's live worktree (the source of truth)
	PrivateStorePath string // the sandbox's private store, laid into the staged tree
	StateDir         string // sandbox state dir: holds the manifest and the staged trees
	GuestTree        string // the tree the guest actually sees
	Force            bool   // overwrite guest-modified paths (each one is reported)
	// DryRun classifies without applying: the same staging build and the same
	// collision policy run, then the Result is returned and the guest's tree
	// and the delivery baseline are both left exactly as they were.
	// Refs: MGIT-76
	DryRun bool
}

// Sync propagates host worktree changes into a running sandbox's tree.
//
// It re-stages through internal/sandboxd/staging — the same tested path that
// drops in-worktree stores and REJECTS escaping symlinks — so a sync can never
// deliver something a launch would have refused. It then compares three views
// (delivered / host / guest), applies only what the collision policy permits,
// and records the new baseline.
//
// A host worktree that has not changed since the last delivery costs one
// staging build and no writes, which is what makes an automatic pre-exec sync
// affordable. Refs: MGIT-71, SEC-03, ADR-011
func Sync(req Request) (Result, error) {
	candidate := filepath.Join(req.StateDir, stagedSubdir)
	// Build into a FRESH directory: a leftover tree from a previous sync would
	// make a deleted file look present.
	if err := os.RemoveAll(candidate); err != nil {
		return Result{}, fmt.Errorf("worktree sync: clear candidate tree: %w", err)
	}
	defer func() { _ = os.RemoveAll(candidate) }()
	if err := staging.Build(req.WorktreePath, req.PrivateStorePath, candidate); err != nil {
		// Fails closed exactly as a launch would: an escaping symlink or an
		// unreadable worktree refuses the sync rather than delivering part of
		// a tree. Refs: SEC-03
		return Result{}, fmt.Errorf("worktree sync: re-stage: %w", err)
	}

	host, delivered, guest, err := views(req, candidate)
	if err != nil {
		return Result{}, err
	}

	plan := Compute(delivered, host, guest)
	if plan.Empty() {
		return Result{Skipped: true, DryRun: req.DryRun}, nil
	}
	if req.Force {
		plan = plan.Forced()
	}
	if req.DryRun {
		// A query stops HERE — after the same staging build and the same
		// collision policy a real sync runs, which is what makes the report
		// trustworthy, but before anything is written. The guest's tree and
		// the delivery baseline are both left exactly as they were.
		// Refs: MGIT-76
		return classify(plan, true), nil
	}
	if plan.Blocked() {
		return Result{}, &ConflictError{Conflicts: plan.Conflicts}
	}
	if err := Apply(candidate, req.GuestTree, plan); err != nil {
		return Result{}, err
	}
	// READ THE DELIVERY BACK before reporting it.
	//
	// A sync once reported success while the guest's tree lacked a created
	// file, and mgit had no idea — the drop was caught by a consumer's
	// stale-copy check one layer up (MGIT-164). Apply returning nil says the
	// writes were attempted; it does not say the guest can read them.
	//
	// The check costs one more manifest build of a tree already on local disk,
	// and it is the difference between trusting the apply and knowing it. It
	// runs BEFORE the baseline moves, so an undelivered sync re-derives the
	// same work next time instead of recording a delivery that did not happen.
	// Refs: MGIT-164
	landed, err := BuildManifest(req.GuestTree)
	if err != nil {
		return Result{}, fmt.Errorf("worktree sync: read back the guest tree: %w", err)
	}
	if err := VerifyDelivery(plan, host, landed); err != nil {
		return Result{}, err
	}
	// The baseline moves only after a VERIFIED apply, so a failed sync
	// re-derives the same work next time rather than losing track of it.
	if err := SaveManifest(req.StateDir, host); err != nil {
		return Result{}, err
	}
	return classify(plan, false), nil
}

// views gathers the three trees the collision policy compares: what the host
// HAS now (the freshly staged candidate), what it last DELIVERED, and what the
// GUEST currently has.
func views(req Request, candidate string) (host, delivered, guest Manifest, err error) {
	if host, err = BuildManifest(candidate); err != nil {
		return nil, nil, nil, err
	}
	if delivered, err = LoadManifest(req.StateDir); err != nil {
		return nil, nil, nil, err
	}
	if guest, err = BuildManifest(req.GuestTree); err != nil {
		return nil, nil, nil, err
	}
	return host, delivered, guest, nil
}

// classify renders a computed plan as a Result. dryRun records that nothing
// was applied, so one shape describes a delivery and a projection without the
// two being confusable. Refs: MGIT-76
func classify(plan Plan, dryRun bool) Result {
	return Result{
		Updated: plan.Update, Deleted: plan.Delete, Overridden: plan.Overridden(),
		Conflicts: plan.Conflicts, Blocked: plan.Blocked(), DryRun: dryRun,
	}
}

// RecordDelivery writes the delivery baseline for a freshly staged tree. The
// backend calls it at launch, right after staging, so the first sync knows
// what the guest started from and can tell a guest edit from a host one.
func RecordDelivery(stagedTree, stateDir string) error {
	manifest, err := BuildManifest(stagedTree)
	if err != nil {
		return err
	}
	return SaveManifest(stateDir, manifest)
}

// LoadManifest reads the delivery baseline. An absent manifest is an empty
// one: a sandbox launched before this existed simply has every host path look
// new, and the collision policy still protects anything the guest has since
// created or modified.
func LoadManifest(stateDir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, manifestName)) //nolint:gosec // manager-owned state dir
	if os.IsNotExist(err) {
		return Manifest{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worktree sync: read delivery manifest: %w", err)
	}
	var out Manifest
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("worktree sync: parse delivery manifest: %w", err)
	}
	return out, nil
}

// SaveManifest writes the delivery baseline atomically, so an interrupted
// write cannot leave a half-parsed manifest that would misclassify every path.
func SaveManifest(stateDir string, manifest Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".sync-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(stateDir, manifestName))
}

// SortedPaths is a helper for stable audit/log output.
func SortedPaths(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
