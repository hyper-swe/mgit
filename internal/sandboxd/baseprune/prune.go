// Package baseprune decides which guest-base cache entries nothing pins any
// more, and removes exactly those. Refs: MGIT-239
package baseprune

import (
	"context"
	"errors"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// Verdict is what prune concludes about one entry.
type Verdict string

// Verdicts.
const (
	Prunable   Verdict = "prunable"
	Pinned     Verdict = "pinned"
	InUse      Verdict = "in use"
	Unknown    Verdict = "pinner unknown"
	CannotTell Verdict = "cannot tell"
)

// Pinner states.
const (
	PinnerPins     = "pins"
	PinnerRepinned = "re-pinned"
	PinnerGone     = "gone"
)

// PinnerState is one recorded pinner and what its lock says now.
type PinnerState struct {
	Root  string
	State string
}

// Item is one entry with its verdict.
type Item struct {
	Digest, Path, ComposedBy, Reason string
	Bytes                            int64
	Pinners                          []PinnerState
	Verdict                          Verdict
}

// Deps are the facts prune consults.
type Deps struct {
	Cache    *basecache.Cache
	LockPins func(hostRoot string) (map[string]bool, error)
	InUse    func(ctx context.Context) (map[string]bool, error)
}

// Result is what Apply did.
type Result struct {
	Items   []Item
	Removed []Item
	Freed   int64
}

var errNotYet = errors.New("base prune: not built yet")

// Plan judges every entry.
func Plan(ctx context.Context, d Deps) ([]Item, error) { return nil, errNotYet }

// Apply removes the prunable entries.
func Apply(ctx context.Context, d Deps, unknown []string) (Result, error) { return Result{}, errNotYet }
