package images

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNoWarmSnapshot indicates the pool holds no clean-base snapshot
// for an image digest (the caller cold-boots instead).
var ErrNoWarmSnapshot = errors.New("no warm snapshot available")

// Snapshotter boots a clean base image to its pre-exec point and
// captures a restorable snapshot. The pool is the ONLY caller, so a
// used guest can never enter the pool (SEC-08/FR-17.25).
//
// The returned []byte is the snapshot's restorable memory image. At
// real guest scale (multi-GB) a platform Snapshotter SHOULD back this
// with an mmap'd file rather than a heap buffer — the pool only
// zeroes and hands off the slice, so a file-backed mmap satisfies the
// SEC-08 zeroing contract without GB-scale heap residency. Tracked for
// the platform backends (MGIT-11.5.1 / MGIT-11.5.2 snapshotters).
type Snapshotter interface {
	SnapshotCleanBase(ctx context.Context, imageDigest string) ([]byte, error)
}

// Provenance proves a snapshot's clean-base origin. The pool stamps it
// itself; there is no API to set it from outside (SEC-08).
type Provenance struct {
	Source      string    // always "clean-base-boot"
	ImageDigest string    // the base image the snapshot came from
	CreatedAt   time.Time // when the clean base was snapshotted
}

// cleanBaseSource is the only provenance source the pool ever stamps.
const cleanBaseSource = "clean-base-boot"

// Snapshot is one restorable warm-start image plus its provenance.
type Snapshot struct {
	Provenance Provenance
	Memory     []byte // restorable guest memory image
}

// Release zeroes the snapshot's memory before it returns to the host:
// reclaimed pages must not leak prior content (SEC-08).
func (s *Snapshot) Release() {
	zero(s.Memory)
}

// WarmPool holds at most one clean-base snapshot per image digest for
// sub-200ms warm starts (NFR-17.2). Snapshots are created only by
// Prime (which boots a clean base) and consumed by Acquire; a consumed
// snapshot is never handed out twice. Refs: SEC-08, FR-17.25, NFR-17.2
type WarmPool struct {
	snapshotter Snapshotter
	clock       func() time.Time

	mu        sync.Mutex
	snapshots map[string]*Snapshot
}

// NewWarmPool creates an empty pool.
func NewWarmPool(snapshotter Snapshotter, clock func() time.Time) *WarmPool {
	return &WarmPool{
		snapshotter: snapshotter,
		clock:       clock,
		snapshots:   make(map[string]*Snapshot),
	}
}

// Prime boots the clean base for imageDigest and stores a snapshot
// stamped with clean-base provenance. A prior primed snapshot for the
// same digest is discarded (zeroed) first. Refs: SEC-08, FR-17.25
func (p *WarmPool) Prime(ctx context.Context, imageDigest string) error {
	memory, err := p.snapshotter.SnapshotCleanBase(ctx, imageDigest)
	if err != nil {
		return err
	}
	snap := &Snapshot{
		Provenance: Provenance{
			Source:      cleanBaseSource,
			ImageDigest: imageDigest,
			CreatedAt:   p.clock().UTC(),
		},
		Memory: memory,
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.snapshots[imageDigest]; ok {
		prev.Release()
	}
	p.snapshots[imageDigest] = snap
	return nil
}

// Acquire consumes and returns the warm snapshot for imageDigest, or
// ErrNoWarmSnapshot if none is primed. A consumed snapshot leaves the
// pool: the next caller re-primes from a clean base, never reusing a
// dispensed snapshot. Refs: SEC-08
func (p *WarmPool) Acquire(_ context.Context, imageDigest string) (*Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap, ok := p.snapshots[imageDigest]
	if !ok {
		return nil, ErrNoWarmSnapshot
	}
	delete(p.snapshots, imageDigest)
	return snap, nil
}

// Discard drops a primed-but-unacquired snapshot, zeroing its memory.
func (p *WarmPool) Discard(imageDigest string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if snap, ok := p.snapshots[imageDigest]; ok {
		snap.Release()
		delete(p.snapshots, imageDigest)
	}
}

// zero overwrites a byte slice in place.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
