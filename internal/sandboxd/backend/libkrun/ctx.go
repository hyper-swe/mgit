package libkrun

import (
	"errors"
	"fmt"
	"math"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// rootFSTag is libkrun's well-known virtiofs tag for the guest root
// (KRUN_FS_ROOT_TAG in libkrun.h).
const rootFSTag = "/dev/root"

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
	// AddVirtiofs shares a host directory into the guest under a tag
	// (krun_add_virtiofs3; the rootFSTag tag is the guest root).
	AddVirtiofs(ctx uint32, tag, hostDir string, readOnly bool) error
	// SetWorkdir sets the workload's working directory, guest-root-relative.
	SetWorkdir(ctx uint32, dir string) error
	// EnsureVsock makes a vsock device exist on the context, with TSI socket
	// hijacking OFF. It must be called before any vsock port is added:
	// libkrun refuses those with ENODEV when the context has no vsock device.
	// An already-present device is success, not an error. Refs: ADR-010
	EnsureVsock(ctx uint32) error
	// AddVsockPort maps a guest vsock port to a host unix socket path
	// (krun_add_vsock_port2). hostInitiates=true means the host connects in
	// (libkrun listens on the path); false means the guest dials out (libkrun
	// connects to a daemon-owned listener).
	AddVsockPort(ctx uint32, port uint32, socketPath string, hostInitiates bool) error
	// SetExec sets the guest PID-1 workload. args are the arguments ONLY —
	// libkrun prepends the executable itself — and env is passed explicitly
	// even when empty, because a NULL envp would inject the calling process's
	// environment into the guest (SEC-05).
	SetExec(ctx uint32, path string, args, env []string) error
	// StartEnter boots the VM and NEVER RETURNS on success: libkrun seizes
	// the process and exit()s with the guest's exit code at shutdown. A
	// return is always a pre-boot failure. Refs: ADR-010
	StartEnter(ctx uint32) error
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

// newGuestCtx creates and fully configures a libkrun context for one launch,
// from the network wiring through filesystem shares, control-plane vsock
// ports and the guest workload.
//
// Order matters: the network wiring is resolved BEFORE libkrun is touched (an
// unresolvable mode must not leave a context allocated), and the NIC is
// attached IMMEDIATELY after the context exists, so no window exists in which
// a context is live without one. Every failure after creation frees the
// context rather than leaking it. Refs: FR-17.7, SEC-04, ADR-010
func newGuestCtx(api krunAPI, spec vmSpec, deps netDeps) (*guestCtx, error) {
	// Resolve first: fail closed without allocating anything.
	backing, err := netBackingFor(spec.SandboxID, spec.NetworkMode, spec.StateDir)
	if err != nil {
		return nil, err
	}

	// Bind the host end BEFORE the context exists, so a context can never
	// carry a NIC whose peer is missing — that combination boots into a hang
	// rather than failing, which is worse than not launching (ADR-010).
	peer, err := bindHostPeer(backing, deps)
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

	if err := api.SetVMConfig(id, vcpuCount(spec.CPUs), memoryMiB(spec.MemoryMB)); err != nil {
		return nil, gc.abort(fmt.Errorf("libkrun set vm config: %w", err))
	}
	if err := gc.configureGuest(spec); err != nil {
		return nil, gc.abort(err)
	}
	return gc, nil
}

// configureGuest applies the non-network guest configuration: the virtiofs
// root (and worktree share), working directory, control-plane vsock ports,
// and the PID-1 workload. Split from newGuestCtx only to keep each function
// within the complexity budget; it is not independently callable with an
// unconfigured NIC. Refs: FR-17.3, FR-17.11, FR-17.17
func (g *guestCtx) configureGuest(spec vmSpec) error {
	if err := g.api.AddVirtiofs(g.id, rootFSTag, spec.RootDir, spec.RootReadOnly); err != nil {
		return fmt.Errorf("libkrun share guest root %s: %w", spec.RootDir, err)
	}
	workdir := "/"
	if spec.WorktreeHostDir != "" {
		// The SOURCE is the staged tree (SEC-03); the guest mounts it at the
		// identical path and works there.
		if err := g.api.AddVirtiofs(g.id, spec.WorktreeTag, spec.WorktreeHostDir, false); err != nil {
			return fmt.Errorf("libkrun share worktree %s: %w", spec.WorktreeHostDir, err)
		}
		workdir = spec.WorktreePath
	}
	if err := g.api.SetWorkdir(g.id, workdir); err != nil {
		return fmt.Errorf("libkrun set workdir: %w", err)
	}
	// A vsock device must exist before any port can be mapped onto it, and
	// libkrun does NOT always create one implicitly: on macOS it pre-creates a
	// TSI vsock at context creation, but on Linux — where our explicit NIC has
	// already turned TSI off — it creates none, and every port add then fails
	// ENODEV. Asking for one explicitly is what makes the control plane
	// portable. Refs: ADR-010, FR-17.11
	if spec.VsockEnabled || len(spec.PublishPorts) > 0 {
		if err := g.api.EnsureVsock(g.id); err != nil {
			return fmt.Errorf("libkrun enable vsock: %w", err)
		}
	}
	if spec.VsockEnabled {
		for _, port := range controlVsockPorts() {
			// Only the notify port is guest-initiated: the guest dials the
			// host's land-ready listener; exec and land are host-initiated.
			hostInitiates := port != microvm.GuestNotifyPort
			if err := g.api.AddVsockPort(g.id, port, vsockSocketPath(spec.StateDir, port), hostInitiates); err != nil {
				return fmt.Errorf("libkrun add vsock port %d: %w", port, err)
			}
		}
	}
	// SEC-09: each published guest port becomes a LISTENING vsock port, so
	// libkrun accepts host-initiated connections and forwards them IN. The
	// direction is the control: nothing here opens a guest->host path.
	for _, port := range spec.PublishPorts {
		p, err := publishVsockPort(port)
		if err != nil {
			return err
		}
		if err := g.api.AddVsockPort(g.id, p, vsockSocketPath(spec.StateDir, p), true); err != nil {
			return fmt.Errorf("libkrun publish guest port %d: %w", port, err)
		}
	}
	if err := g.api.SetExec(g.id, spec.ExecPath, spec.ExecArgs, spec.ExecEnv); err != nil {
		return fmt.Errorf("libkrun set guest exec: %w", err)
	}
	return nil
}

// enter boots the configured VM. On success it NEVER RETURNS: libkrun seizes
// the process and exit()s with the guest's exit code when the VM shuts down
// (ADR-010) — which is why only the re-exec child calls this, never the
// daemon. A return is a pre-boot failure; the context and host peer are
// released before it surfaces.
func (g *guestCtx) enter() error {
	err := g.api.StartEnter(g.id)
	// Unreachable after a successful boot; anything here failed pre-boot.
	return g.abort(fmt.Errorf("libkrun start vm: %w", err))
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
