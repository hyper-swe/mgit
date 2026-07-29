// Package libkrun is the cross-platform microVM backend built on libkrun
// (Apache-2.0): the VMM links into the daemon and boots the guest kernel
// bundled by libkrunfw, giving one hypervisor across Linux/KVM and
// macOS/HVF.
//
// The backend plugs into the microvm.Hypervisor seam, so the lifecycle,
// exec, land and notify orchestration come from the shared microvm.Manager
// rather than being reimplemented here.
//
// ONE VM PER PROCESS. krun_start_enter never returns: it seizes the calling
// process's stdio and exit()s with the guest's exit code when the VM shuts
// down. So VMs cannot run inside the daemon — Hypervisor re-execs the daemon
// binary per VM (ChildCommand), and the child builds the context, hosts the
// VM's host-side network peer, and enters the VM. See ADR-010.
//
// STATUS (MGIT-61.6/61.8): in place — the host-side network decision, the
// CGO binding, the NIC + host-peer invariant, the netstack egress gateway
// with TCP policy enforcement and the SEC-07 pinning DNS resolver, the
// virtiofs root and the SEC-03 staged worktree share, the control-plane vsock
// ports, SEC-09 publishing over libkrun's listening vsock ports, the re-exec
// lifecycle, and the guest dialers. All three network modes are served:
// none by the discard socket, allowlist by the standard egress assembly, and
// open by an allow-all authorizer that still audits every flow.
//
// Validated on real hardware (macOS/HVF): boot, none-mode deny, allowlist
// default-deny, host->guest SSH over a published port, an agent running the
// mgit CLI against the SEC-03 private store, concurrent launches with no
// cross-task contamination, and virtio-fs perf on a real dependency tree.
//
// NOT YET:
//   - Linux/KVM validation. Everything above was proven on macOS/HVF only;
//     the tagged build and unit tests pass on KVM but the real-VM boot does
//     not (MGIT-61.13 P4).
//
// PREREQUISITE: the linked libkrun must be built WITH networking (upstream
// `make NET=1`). Every sandbox gets an explicit NIC in every mode, because a
// VM without one gets TSI and full host egress — so a libkrun lacking the
// net API cannot host a sandbox at all, and NewHypervisor refuses it.
// Refs: FR-17.15, ADR-005, ADR-010, MGIT-61.14
package libkrun

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// Host-side backend socket names, per sandbox state dir. Both are unix
// sockets the VMM connects its virtio-net device to; which one a launch gets
// is the whole of the egress decision.
const (
	// denySocketName backs "none" mode. Its host end is a bound, draining
	// discard socket (discardSocket): the guest's NIC has a willing peer but
	// no route anywhere. An unserved path would hang the VM, not close it.
	denySocketName = "net-deny.sock"
	// proxySocketName backs allowlist/open mode. Its host end is netGateway:
	// a userspace TCP/IP stack that terminates the guest's connections and
	// admits only what the egress authorizer allows.
	proxySocketName = "net-proxy.sock"
)

// netBacking is how one sandbox's virtio-net device is wired on the host.
//
// SECURITY INVARIANT (ADR-010): SocketPath is ALWAYS non-empty. libkrun
// auto-enables TSI (Transparent Socket Impersonation) when a VM is
// configured with no network device, which transparently proxies the guest's
// sockets through the host — so "add no NIC" is a full egress leak, the exact
// opposite of closed. Adding an explicit device disables TSI. Every mode
// therefore gets a device, and every device gets a bound host peer.
// Refs: FR-17.7, SEC-04, ADR-010
type netBacking struct {
	Mode       string // model.NetworkMode*
	SocketPath string // host unix socket the VMM's NIC connects to; never empty
	MAC        string // deterministic per-sandbox locally-administered address
}

// Deny reports that the backing intentionally carries no traffic: the device
// exists only to keep libkrun off its TSI fallback, and its host end is a
// draining discard socket (see discardSocket) rather than a real network.
func (n netBacking) Deny() bool { return n.Mode == model.NetworkModeNone }

// parseMAC converts the backing's textual address into the six bytes
// krun_add_net_* takes. It lives in Go rather than the CGO binding so the
// failure path stays testable on a host without libkrun. Refs: SEC-05
func parseMAC(mac string) ([6]byte, error) {
	var out [6]byte
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return out, fmt.Errorf("parse guest MAC %q: %w", mac, err)
	}
	if len(hw) != len(out) {
		return out, fmt.Errorf("guest MAC %q is %d bytes, want %d (EUI-48)",
			mac, len(hw), len(out))
	}
	copy(out[:], hw)
	return out, nil
}

// netBackingFor resolves the NIC wiring for one launch: every mode returns a
// backing with an explicit SocketPath, and anything unresolvable is an error
// rather than a NIC-less VM.
//
// The launch's VMConfig.AttachNIC is deliberately NOT a parameter. It is
// derived from the network mode for backends whose "no device" default is
// fail-closed (vzf, firecracker); on libkrun that default is fail-OPEN (TSI),
// so honoring AttachNIC=false would be an egress leak, not a closed network.
//
// This decides the wiring; newGuestCtx enforces it by attaching the NIC
// before any context is handed out. The remaining proof that TSI is actually
// off is the egress e2e that probes a real guest. Refs: FR-17.7, FR-17.8, SEC-04, ADR-010
func netBackingFor(sandboxID, mode, stateDir string) (netBacking, error) {
	if stateDir == "" {
		return netBacking{}, fmt.Errorf(
			"%w: libkrun needs a sandbox state dir to place the net backing socket; "+
				"without one the VM would boot with libkrun's TSI fallback and leak host egress",
			model.ErrSandboxBackendUnavailable)
	}

	backing := netBacking{Mode: mode, MAC: microvm.GuestMAC(sandboxID)}
	switch mode {
	case model.NetworkModeNone:
		backing.SocketPath = filepath.Join(stateDir, denySocketName)
	case model.NetworkModeAllowlist, model.NetworkModeOpen:
		backing.SocketPath = filepath.Join(stateDir, proxySocketName)
	default:
		return netBacking{}, fmt.Errorf(
			"%w: unknown network mode %q for the libkrun backend",
			model.ErrSandboxBackendUnavailable, mode)
	}

	// Checked here so BOTH socket paths are covered, and at resolve time —
	// before anything is bound or allocated. bind(2) would otherwise report a
	// bare "invalid argument" with no hint at the real cause.
	if err := checkSocketPathLen("net backing socket", backing.SocketPath); err != nil {
		return netBacking{}, err
	}
	return backing, nil
}

// checkSocketPathLen rejects a unix socket path over the portable sun_path
// ceiling, naming which socket it is. One helper for every socket the backend
// binds, so the limit and its remedy cannot drift per call site.
func checkSocketPathLen(kind, path string) error {
	if len(path) <= maxUnixSocketPath {
		return nil
	}
	return fmt.Errorf(
		"%w: %s path is %d bytes, over the %d-byte unix socket limit: %s "+
			"(use a shorter sandbox state directory)",
		model.ErrSandboxBackendUnavailable, kind, len(path), maxUnixSocketPath, path)
}

// daxWindowEnv overrides the virtio-fs DAX window size in bytes, for
// measuring its effect (ADR-010 Gate 2). It is an env override rather than a
// config field because it is a MEASUREMENT knob: the shipped value is the
// constant below, and a per-launch setting would invite tuning a containment
// component per sandbox.
const daxWindowEnv = "MGIT_LIBKRUN_DAX_BYTES"

// defaultDAXWindow is the virtio-fs DAX window the backend ships with.
//
// DAX maps file pages into a shared memory region the guest reads directly,
// instead of round-tripping every read through the virtio queue. Zero
// disables it. The value here is the measured default — see ADR-010 Gate 2
// for the numbers behind it. Refs: ADR-010, NFR-17.2
const defaultDAXWindow = 0

// virtiofsDAXWindow resolves the DAX window size for a share. An unparseable
// or negative override is ignored rather than failing the launch: this is a
// performance knob, and a bad value must not cost a user their sandbox.
func virtiofsDAXWindow() uint64 {
	raw := os.Getenv(daxWindowEnv)
	if raw == "" {
		return defaultDAXWindow
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return defaultDAXWindow
	}
	return n
}
