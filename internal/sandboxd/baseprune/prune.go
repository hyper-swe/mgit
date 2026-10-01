// Package baseprune decides which entries of the machine-wide guest-base
// cache nothing pins any more, and removes exactly those.
//
// The cache never rewrites an entry, so without eviction it only grows: on
// 2026-09-24 a full-disk stop found 35 entries (14.5 GiB) of which a scan
// could tie only 8 to any images.lock. A scan is a guess bounded by where it
// looked, so prune does not scan. It reads the back-references recorded when
// each entry was pinned (basecache.RecordPinner) and asks each recorded
// repository's CURRENT lock whether the pin still holds.
//
// THE RULE. An entry is removed only when every recorded pinner has stopped
// pinning it or no longer exists, and no sandbox runs on it. Anything prune
// cannot establish — a lock it cannot read, a daemon it cannot ask — keeps
// the entry and says why: "cannot tell" is a verdict of its own, never a
// quiet yes. An entry with no back-reference at all predates the records; its
// pinners are unknown, so it is removed only when named explicitly.
// Refs: MGIT-239
package baseprune

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
)

// Verdict is what prune concludes about one entry.
type Verdict string

// The verdicts. Only Prunable is removed without being named; Unknown is
// removed only when named; the rest are never removed.
const (
	Prunable   Verdict = "prunable"
	Pinned     Verdict = "pinned"
	InUse      Verdict = "in use"
	Unknown    Verdict = "pinner unknown"
	CannotTell Verdict = "cannot tell"
)

// What a recorded pinner's lock says about the entry now.
const (
	PinnerPins     = "pins"       // the lock still pins this digest
	PinnerRepinned = "re-pinned"  // the repository exists and pins something else
	PinnerGone     = "gone"       // the repository was removed from a directory that still exists
	PinnerMissing  = "missing"    // gone along with its parent: moved, or on a volume not mounted now
	PinnerUnread   = "unreadable" // its lock could not be read
)

// PinnerState is one recorded pinner and what its lock says now.
type PinnerState struct {
	Root  string
	State string
}

// Item is one cache entry with everything prune knows about it.
type Item struct {
	Digest     string
	Path       string
	Bytes      int64
	ComposedBy string // the mgit version that composed it, or "unknown"
	Pinners    []PinnerState
	// FullyRecorded is the publisher's assertion that every pinner records
	// itself. Without it, records name SOME pinners and never all of them.
	FullyRecorded bool
	Verdict       Verdict
	Reason        string // why, in words, for every verdict but Prunable
}

// Deps are the facts prune consults, injected so each can be held to a test.
type Deps struct {
	Cache *basecache.Cache
	// LockPins reads a host root's CURRENT images.lock: the digests it pins
	// from the cache. Production passes images.CachedPins.
	LockPins func(hostRoot string) (map[string]bool, error)
	// InUse asks every live sandbox daemon which digests its sandboxes boot
	// from, and which repositories they serve. An error means at least one
	// daemon could not be asked.
	InUse func(ctx context.Context) (Live, error)
	// ReleasedRoots are recorded roots the operator says were deleted.
	ReleasedRoots []string
}

// Live is what the live daemons report.
type Live struct {
	Digests   map[string]bool // bases a created, running or suspended sandbox boots from
	HostRoots []string        // the sandbox config roots of the repositories they serve
}

// view is everything one judgement reads besides the entry itself: the
// daemons' answer, and every repository known to this machine whose lock is
// read for EVERY entry, recorded against it or not.
type view struct {
	live    Live
	liveErr error
	known   []string
}

// Result is what Apply did.
type Result struct {
	Items   []Item // every entry, with its verdict at plan time
	Removed []Item // the entries removed, in digest order
	Freed   int64  // bytes freed by those removals
}

// Plan judges every entry in the cache and changes nothing — the whole of
// `--dry-run`. Refs: MGIT-239
func Plan(ctx context.Context, d Deps) ([]Item, error) {
	listed, err := d.Cache.Entries()
	if err != nil {
		return nil, err
	}
	v, err := look(ctx, d, listed)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(listed))
	for _, l := range listed {
		it, err := judge(d, l, v)
		if err != nil {
			return nil, err
		}
		if it.Bytes, err = basecache.TreeBytes(l.Path); err != nil {
			return nil, err
		}
		it.ComposedBy = "unknown"
		if cb, cbErr := guestbase.ReadComposedBy(l.Path); cbErr == nil {
			it.ComposedBy = cb.Version
		}
		items = append(items, it)
	}
	return items, nil
}

// look asks the daemons, and gathers every repository this machine knows of:
// each live daemon's, and each one recorded against ANY entry. Reading all of
// their locks for every entry is a net under the records, not a replacement
// for them: it catches a pin that was never recorded against this entry by a
// repository prune can name for another reason.
func look(ctx context.Context, d Deps, listed []basecache.Listed) (view, error) {
	live, liveErr := d.InUse(ctx)
	seen := map[string]bool{}
	var known []string
	add := func(root string) {
		if !seen[root] {
			seen[root] = true
			known = append(known, root)
		}
	}
	for _, root := range live.HostRoots {
		add(root)
	}
	for _, l := range listed {
		roots, _, err := d.Cache.Pinners(l.Digest)
		if err != nil {
			return view{}, err
		}
		for _, root := range roots {
			add(root)
		}
	}
	return view{live: live, liveErr: liveErr, known: known}, nil
}

// judge reaches one entry's verdict. The order is the rule: a live pin first,
// then anything prune cannot establish, then a running sandbox, then whether
// the records can be trusted to name every pinner at all.
func judge(d Deps, l basecache.Listed, v view) (Item, error) {
	it := Item{Digest: l.Digest, Path: l.Path, FullyRecorded: d.Cache.FullyRecorded(l.Digest)}
	roots, recorded, err := d.Cache.Pinners(l.Digest)
	if err != nil {
		return Item{}, err
	}
	live, unsure := it.readPinners(d, roots, v.known)
	switch {
	case len(live) > 0:
		it.Verdict, it.Reason = Pinned, "pinned by "+strings.Join(live, ", ")
	case len(unsure) > 0:
		it.Verdict, it.Reason = CannotTell, "cannot read whether these still pin it: "+strings.Join(unsure, ", ")
	case v.liveErr != nil:
		it.Verdict, it.Reason = CannotTell, "cannot tell whether a sandbox runs on it: "+v.liveErr.Error()
	case v.live.Digests[l.Digest]:
		it.Verdict, it.Reason = InUse, "a sandbox is running on it"
	case !it.FullyRecorded && recorded:
		it.Verdict, it.Reason = Unknown, "composed before every pinner was recorded; others may pin it"
	case !it.FullyRecorded:
		it.Verdict, it.Reason = Unknown, "composed before pinners were recorded; nothing says who pins it"
	default:
		it.Verdict = Prunable
	}
	return it, nil
}

// readPinners reads each recorded pinner's lock into it.Pinners, and each
// other known repository's lock too. It returns the roots that pin the entry
// and the roots whose answer could not be read.
func (it *Item) readPinners(d Deps, recorded, known []string) (live, unsure []string) {
	isRecorded := map[string]bool{}
	for _, root := range recorded {
		isRecorded[root] = true
		state := pinnerState(d, root, it.Digest)
		it.Pinners = append(it.Pinners, PinnerState{Root: root, State: state})
		switch state {
		case PinnerPins:
			live = append(live, root)
		case PinnerUnread, PinnerMissing:
			unsure = append(unsure, root)
		}
	}
	for _, root := range known {
		if isRecorded[root] {
			continue
		}
		switch pinnerState(d, root, it.Digest) {
		case PinnerPins:
			live = append(live, root+" (not recorded against this entry)")
		case PinnerUnread:
			unsure = append(unsure, root)
		}
	}
	return live, unsure
}

// pinnerState asks one root's current lock about digest. A root that no
// longer exists is GONE only when the directory that held its repository
// still exists; when that is missing too, the repository may have moved or
// sit on a volume that is not mounted, and its pin cannot be read as released.
func pinnerState(d Deps, root, digest string) string {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		repoParent := filepath.Dir(filepath.Dir(filepath.Dir(root))) // root is <repo>/.mgit/sandbox
		if _, perr := os.Stat(repoParent); perr == nil {
			return PinnerGone
		}
		return PinnerMissing
	}
	pins, err := d.LockPins(root)
	if err != nil {
		return PinnerUnread
	}
	if pins[digest] {
		return PinnerPins
	}
	return PinnerRepinned
}

// Apply removes every Prunable entry, plus each entry named in unknown whose
// pinners are unknown, and reports the bytes freed.
//
// Naming is the explicit, per-entry consent the ticket requires for an entry
// whose pinners nobody can vouch for. It is refused for a fully recorded
// entry, and it never overrides a live pin, a running sandbox or a
// cannot-tell: a named entry is removed only when its verdict is Unknown. Each entry's verdict is taken
// AGAIN immediately before it is removed, so a repository that pins it in the
// meantime keeps it. Refs: MGIT-239
func Apply(ctx context.Context, d Deps, unknown []string) (Result, error) {
	items, err := Plan(ctx, d)
	if err != nil {
		return Result{}, err
	}
	named, err := checkNamed(items, unknown)
	if err != nil {
		return Result{}, err
	}
	res := Result{Items: items}
	for _, it := range items {
		if !removable(it.Verdict, named[it.Digest]) {
			continue
		}
		again, err := rejudge(ctx, d, it)
		if err != nil {
			return res, err
		}
		if !removable(again.Verdict, named[it.Digest]) {
			continue
		}
		if err := d.Cache.Remove(it.Digest); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, it)
		res.Freed += it.Bytes
	}
	return res, nil
}

// removable is the whole removal rule in one place.
func removable(v Verdict, named bool) bool {
	return v == Prunable || (v == Unknown && named)
}

// rejudge takes one entry's verdict again, asking the daemons and every
// known repository afresh.
func rejudge(ctx context.Context, d Deps, it Item) (Item, error) {
	listed, err := d.Cache.Entries()
	if err != nil {
		return Item{}, err
	}
	v, err := look(ctx, d, listed)
	if err != nil {
		return Item{}, err
	}
	return judge(d, basecache.Listed{Digest: it.Digest, Path: it.Path}, v)
}

// checkNamed refuses, before anything is removed, a name that is not in the
// cache or that names a fully recorded entry: a typo, or a digest the records
// can already judge, must fail loudly, not quietly do less than asked.
func checkNamed(items []Item, unknown []string) (map[string]bool, error) {
	byDigest := make(map[string]Item, len(items))
	for _, it := range items {
		byDigest[it.Digest] = it
	}
	named := make(map[string]bool, len(unknown))
	for _, digest := range unknown {
		it, ok := byDigest[digest]
		if !ok {
			return nil, fmt.Errorf("base prune: %s is not in the cache", digest)
		}
		if it.FullyRecorded {
			return nil, fmt.Errorf("base prune: %s records every pinner (%s); naming an entry "+
				"removes it only when its pinners are not all known", digest, it.Verdict)
		}
		named[digest] = true
	}
	return named, nil
}
