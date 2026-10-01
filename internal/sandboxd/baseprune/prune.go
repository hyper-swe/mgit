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
	PinnerGone     = "gone"       // the repository's sandbox config root no longer exists
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
	Verdict    Verdict
	Reason     string // why, in words, for every verdict but Prunable
}

// Deps are the facts prune consults, injected so each can be held to a test.
type Deps struct {
	Cache *basecache.Cache
	// LockPins reads a host root's CURRENT images.lock: the digests it pins
	// from the cache. Production passes images.CachedPins.
	LockPins func(hostRoot string) (map[string]bool, error)
	// InUse asks every live sandbox daemon which digests its sandboxes boot
	// from. An error means at least one daemon could not be asked.
	InUse func(ctx context.Context) (map[string]bool, error)
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
	inUse, inUseErr := d.InUse(ctx)
	items := make([]Item, 0, len(listed))
	for _, l := range listed {
		it, err := judge(d, l, inUse, inUseErr)
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

// judge reaches one entry's verdict from its back-references, its pinners'
// current locks and the daemons' answer. The order is the rule: a live pin
// first, then anything prune cannot establish, then a running sandbox.
func judge(d Deps, l basecache.Listed, inUse map[string]bool, inUseErr error) (Item, error) {
	it := Item{Digest: l.Digest, Path: l.Path}
	roots, recorded, err := d.Cache.Pinners(l.Digest)
	if err != nil {
		return Item{}, err
	}
	var live, unread []string
	for _, root := range roots {
		state := pinnerState(d, root, l.Digest)
		it.Pinners = append(it.Pinners, PinnerState{Root: root, State: state})
		switch state {
		case PinnerPins:
			live = append(live, root)
		case PinnerUnread:
			unread = append(unread, root)
		}
	}
	switch {
	case len(live) > 0:
		it.Verdict, it.Reason = Pinned, "pinned by "+strings.Join(live, ", ")
	case len(unread) > 0:
		it.Verdict, it.Reason = CannotTell, "cannot read the images.lock of "+strings.Join(unread, ", ")
	case inUseErr != nil:
		it.Verdict, it.Reason = CannotTell, "cannot tell whether a sandbox runs on it: "+inUseErr.Error()
	case inUse[l.Digest]:
		it.Verdict, it.Reason = InUse, "a sandbox is running on it"
	case !recorded:
		it.Verdict, it.Reason = Unknown, "composed before pinners were recorded; nothing says who pins it"
	default:
		it.Verdict = Prunable
	}
	return it, nil
}

// pinnerState asks one recorded root's current lock about digest.
func pinnerState(d Deps, root, digest string) string {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return PinnerGone
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
// nobody can vouch for. It is refused for an entry that records a pinner, and
// it never overrides a running sandbox or a cannot-tell: a named entry is
// removed only when its verdict is Unknown. Each entry's verdict is taken
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

// rejudge takes one entry's verdict again, asking the daemons afresh.
func rejudge(ctx context.Context, d Deps, it Item) (Item, error) {
	inUse, inUseErr := d.InUse(ctx)
	return judge(d, basecache.Listed{Digest: it.Digest, Path: it.Path}, inUse, inUseErr)
}

// checkNamed refuses, before anything is removed, a name that is not in the
// cache or that names an entry with a recorded pinner: a typo or a pinned
// digest must fail loudly, not quietly do less than the user asked.
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
		if len(it.Pinners) > 0 {
			return nil, fmt.Errorf("base prune: %s records its pinners (%s); naming an entry "+
				"removes it only when nothing records who pins it", digest, it.Verdict)
		}
		named[digest] = true
	}
	return named, nil
}
