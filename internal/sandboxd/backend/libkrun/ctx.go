package libkrun

import (
	"errors"
	"fmt"
	"math"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// krunAPI is the libkrun C surface the backend drives, injected so the
// configuration sequence — above all the mandatory NIC — is testable on any
// host, with or without libkrun installed. The real implementation is the
// CGO binding built under the "libkrun" tag; tests supply a recorder.
//
// It mirrors the C calls 1:1 deliberately: keeping the ORDER in Go (rather
// than behind a coarse Configure()) is what lets a host without libkrun test
// that the NIC is attached first. guestCtx is the only sequencer.
//
// Handles are libkrun's uint32 context IDs. Errors carry the negative errno
// libkrun returns, wrapped with the call that produced it.
type krunAPI interface {
	// CreateCtx allocates a configuration context.
	CreateCtx() (uint32, error)
	// AddNetUnixgram attaches a virtio-net device backed by a host unixgram
	// socket. Attaching ANY net device is what disables libkrun's TSI
	// fallback, so this is a security-critical call, not a feature.
	AddNetUnixgram(ctx uint32, socketPath, mac string) error
	// SetVMConfig applies the vCPU and memory caps.
	SetVMConfig(ctx uint32, vcpus uint8, ramMiB uint32) error
	// FreeCtx releases a context that will not be started.
	FreeCtx(ctx uint32) error
}

// guestCtx is a libkrun configuration context that carries an explicit
// host-backed NIC.
//
// SCOPE OF THE GUARANTEE (be precise — this is a security control): outside
// this package the invariant is absolute, because nothing here is exported
// and newGuestCtx is the only way to obtain a configured context. INSIDE this
// package it rests on newGuestCtx remaining the sole caller of
// krunAPI.CreateCtx — Go's encapsulation floor is the package, so a sibling
// file could in principle mint a raw handle or a composite literal. That
// single-call-site rule is pinned by TestCreateCtx_HasExactlyOneCallSite
// rather than left to discipline.
//
// DECISION (taken with the binding increment): the binding stays in this
// package rather than moving to a sub-package that hides the raw handle. The
// AST pins are the control, and they scan build-tagged files — including the
// CGO binding — so the funnel holds as the package grows.
//
// The context also OWNS the host end of its NIC: a NIC whose peer is missing
// boots into a hang, so the two are acquired and released together.
// Refs: FR-17.7, SEC-04, SEC-10, ADR-010
type guestCtx struct {
	api  krunAPI
	id   uint32
	peer hostNetPeer // host end of the NIC; owned for the context's lifetime
}

// newGuestCtx creates and configures a libkrun context for one launch.
//
// Order matters: the network wiring is resolved BEFORE libkrun is touched (an
// unresolvable mode must not leave a context allocated), and the NIC is
// attached IMMEDIATELY after the context exists, so no window exists in which
// a context is live without one. Every failure after creation frees the
// context rather than leaking it. Refs: FR-17.7, SEC-04, ADR-010
func newGuestCtx(api krunAPI, cfg microvm.VMConfig, stateDir string, auth flowAuthorizer, dns dnsResolver) (*guestCtx, error) {
	// Resolve first: fail closed without allocating anything.
	backing, err := netBackingFor(cfg.SandboxID, cfg.NetworkMode, stateDir)
	if err != nil {
		return nil, err
	}

	// Bind the host end BEFORE the context exists, so a context can never
	// carry a NIC whose peer is missing — that combination boots into a hang
	// rather than failing, which is worse than not launching (ADR-010).
	peer, err := bindHostPeer(backing, auth, dns)
	if err != nil {
		return nil, err
	}

	id, err := api.CreateCtx()
	if err != nil {
		_ = peer.Close()
		return nil, fmt.Errorf("libkrun create context: %w", err)
	}
	gc := &guestCtx{api: api, id: id, peer: peer}

	// The NIC comes first and is mandatory: without it libkrun boots with TSI
	// and proxies the guest's sockets through the host (ADR-010).
	if err := api.AddNetUnixgram(id, backing.SocketPath, backing.MAC); err != nil {
		return nil, gc.abort(fmt.Errorf(
			"libkrun attach net device (%s mode): %w", backing.Mode, err))
	}

	if err := api.SetVMConfig(id, vcpuCount(cfg.CPUs), memoryMiB(cfg.MemoryMB)); err != nil {
		return nil, gc.abort(fmt.Errorf("libkrun set vm config: %w", err))
	}
	return gc, nil
}

// abort releases everything behind a failed configuration and returns the
// original error, so a partially configured context is never left allocated
// and never reachable. Release failures are joined rather than swallowed.
func (g *guestCtx) abort(cause error) error {
	return errors.Join(cause, g.Close())
}

// Close releases the libkrun context and the host end of its NIC. It is the
// teardown path for a context that will not be started (or whose VM has
// stopped): both resources are host-side and must leave no residue (SEC-10).
func (g *guestCtx) Close() error {
	var errs []error
	if err := g.api.FreeCtx(g.id); err != nil {
		errs = append(errs, fmt.Errorf("libkrun free context: %w", err))
	}
	if g.peer != nil {
		if err := g.peer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close host net peer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// vcpuCount resolves the vCPU count into libkrun's uint8 argument.
//
// A non-positive request means "policy default" (model.SandboxPolicy treats 0
// that way), NOT "the smallest possible VM": the defaults are applied on the
// service path, so a direct backend call can still arrive unset, and silently
// booting a 1-vCPU VM would turn a config gap into a mystery. 255 is the
// ceiling the C argument can carry. SandboxLaunchOptions validates the
// request as non-negative. Refs: NFR-17.5
func vcpuCount(cpus int) uint8 {
	if cpus <= 0 {
		cpus = model.DefaultSandboxPolicy().CPUs
	}
	if cpus > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(cpus) //nolint:gosec // OK: bounded above and below immediately above
}

// memoryMiB resolves the guest memory into libkrun's uint32 argument, with
// the same unset-means-default rule as vcpuCount. Refs: NFR-17.5
func memoryMiB(mb int) uint32 {
	if mb <= 0 {
		mb = model.DefaultSandboxPolicy().MemoryMB
	}
	if mb > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(mb) //nolint:gosec // OK: bounded above and below immediately above
}
