// Package microvm is the shared microVM SandboxManager lifecycle used
// by every hardware-isolated backend (macOS vzf, Linux KVM, Windows
// Hyper-V). It owns the platform-agnostic parts — image resolution,
// per-sandbox COW overlay, the FR-17 isolation contract carried in the
// VM config, register-before-return, and residue-free teardown — over
// a small Hypervisor seam each platform implements with its native
// bindings (CGO confined there, core mgit stays pure-Go).
// Refs: FR-17.1, FR-17.3, FR-17.15, FR-17.16, FR-17.17
package microvm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/quarantine"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// defaultOverlaySizeMB sizes the writable overlay when the request
// leaves the disk quota unresolved (NFR-17.5 default).
const defaultOverlaySizeMB = 4096

// Guest vsock ports the in-guest mgit-guest supervisor listens on, and the
// single source every backend's host dialer connects to: the exec channel
// and the land object-pool channel. cmd/mgit-guest defaults its
// --vsock-port / --land-vsock-port flags to these. Sharing one definition
// across the firecracker and vzf dialers keeps a port change from silently
// splitting the host and guest. Refs: FR-17.11, FR-17.5, MGIT-11.9.7
const (
	GuestExecPort uint32 = 1024
	GuestLandPort uint32 = 1025
	// GuestNotifyPort is the HOST vsock port the guest dials to signal
	// "I committed work, ready to land" (the auto-land trigger, MGIT-11.10.11).
	// It is the only guest->host direction: the guest connects to the host
	// (VMADDR_CID_HOST), and the host listens. The notification carries NO land
	// data and asserts NO provenance — it is purely a trigger; the host then
	// runs the EXISTING verified host-initiated land pull. cmd/mgit-guest dials
	// this port. Refs: MGIT-11.10.11, SEC-10, SEC-01
	GuestNotifyPort uint32 = 1026
)

// ImagePaths locates one resolved, digest-pinned guest image. The
// resolver verifies the image (FR-17.17, FR-17.29) before returning.
type ImagePaths struct {
	KernelPath string
	RootfsPath string
	Cmdline    string
}

// VMConfig is the hypervisor-agnostic VM description the shared manager
// builds; each platform translates it to its native configuration. It
// carries the FR-17 isolation contract.
type VMConfig struct {
	SandboxID string // host-assigned lifecycle ID; lets a backend key a live-VM registry to its dialer (vzf, FR-17.16)
	// StateDir is the per-sandbox state directory (SandboxStateDir) every
	// host-side per-VM artifact belongs under, so teardown stays one RemoveAll
	// (FR-17.19). It is carried explicitly because deriving it — vzf's
	// filepath.Dir(OverlayPath) trick — breaks for a backend with no overlay:
	// libkrun boots a virtiofs root, and a derived "." would drop its net
	// backing sockets in the daemon cwd, shared across sandboxes and surviving
	// teardown. Refs: FR-17.19, ADR-010
	StateDir string
	// TaskID is the task this sandbox serves, carried so a backend that
	// enforces egress in the VM's own process (libkrun's re-exec child) can
	// stamp its audit records; the daemon-side backends take it from the
	// service wiring instead. Refs: FR-17.8
	TaskID         string
	CPUs           int
	MemoryMB       int
	KernelPath     string
	RootfsPath     string
	RootfsReadOnly bool // the pinned image is immutable (FR-17.17)
	Cmdline        string
	OverlayPath    string // per-sandbox COW backing file (FR-17.17), pre-sized to the quota
	WorktreePath   string // shared at the identical guest path (FR-17.3)
	// PrivateStorePath is the host directory backing the guest's PRIVATE,
	// sandbox-local mgit object store (SEC-03). The backend delivers it at the
	// guest's <worktree>/.mgit so the guest commits into it; the host shared
	// .mgit is never delivered. Empty when no provisioner is wired (legacy/
	// direct path) — the quarantine control is realized only when set.
	// Refs: SEC-03, FR-17.3, FR-17.5
	PrivateStorePath string
	WorktreeTag      string // mount tag
	// AttachNIC is DERIVED (NetworkMode != none), not authoritative — it is a
	// convenience for backends whose "no device" default is fail-CLOSED (vzf,
	// firecracker). A backend whose default is fail-OPEN must ignore it and key
	// off NetworkMode: libkrun, for one, silently enables TSI when a VM has no
	// net device, so honoring AttachNIC=false there is an egress leak, not a
	// closed network. Refs: FR-17.7, ADR-010
	AttachNIC   bool
	NetworkMode string // model.NetworkMode*: backend wires NAT (open) vs proxy-route (allowlist) vs no NIC (none) (FR-17.7, FR-17.8)
	// NetworkAllowlist is the launch policy's allowlist, verbatim. Backends
	// whose egress enforcement runs in the daemon (firecracker's proxy) get
	// the policy from the service wiring and may ignore this; a backend whose
	// enforcement lives in the VM's own process (libkrun) has only this seam
	// to receive it. Refs: FR-17.8, SEC-04, ADR-010
	NetworkAllowlist []string
	VsockEnabled     bool
	BalloonEnabled   bool
	// PublishPorts are the GUEST TCP ports the guest must expose for one-way
	// host->guest port publishing (SEC-09): the backend puts them on the guest
	// kernel cmdline (guestboot) so mgit-guest runs an AF_VSOCK->TCP-localhost
	// bridge per port, and the host publisher's host->guest vsock connect
	// reaches the guest's own dev server. Host-side only; no path back to the
	// host. Empty when nothing is published. Refs: SEC-09, FR-17.8
	PublishPorts []int
}

// VM is the lifecycle handle the manager drives.
type VM interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, force bool) error
}

// ConsoleTailer is implemented by VMs that capture the guest's console. It is
// OPTIONAL because the capability is per backend, not universal: libkrun and
// firecracker capture a console log per VM, vzf does not. The manager asks for
// a tail only when a launch fails closed, because that console is where the
// guest's OWN startup error already sits — MGIT-89's "write /etc/resolv.conf:
// operation not supported" and MGIT-91's telling silence were both there while
// the operator saw either a success message or a socket path.
// Refs: MGIT-92, MGIT-89
type ConsoleTailer interface {
	// ConsoleTail returns at most maxBytes of the END of the guest console,
	// or "" when this backend captured none.
	ConsoleTail(maxBytes int) string
}

// Hypervisor creates VMs from configs. Implemented per platform with
// native bindings; faked in tests.
type Hypervisor interface {
	CreateVM(cfg VMConfig) (VM, error)
}

// GuestDialer opens a connection to a running guest's exec channel,
// abstracting the vsock dial (the real AF_VSOCK dial on Linux; an
// in-memory pipe in tests) behind the manager, which knows only that it
// gets an io connection to the bound guest. It is optional: when nil,
// Exec reports the transport unavailable rather than faking success — the
// honest state before the platform vsock dialer and real-boot CID
// assignment are wired (e2e, MGIT-11.13). Refs: FR-17.11, FR-17.16, FR-17.27
type GuestDialer interface {
	DialGuest(ctx context.Context, sandboxID string) (net.Conn, error)
}

// GuestPortDialer opens a connection to an ARBITRARY guest vsock port on one
// running sandbox. It is the same per-VM vsock transport the exec/land
// dialers use, only the guest port is a parameter rather than baked in — the
// one-way port publisher (SEC-09) dials a guest dev-server port through it.
// The host->guest direction only: this never opens a guest->host path.
// Refs: SEC-09, FR-17.8, FR-17.16
type GuestPortDialer interface {
	DialGuestPort(ctx context.Context, sandboxID string, guestPort int) (net.Conn, error)
}

// PeerBinder records a sandbox's host-observed peer identity at launch and
// clears it at teardown, so the daemon can authorize incoming guest->host
// land/attestation channels against it (SEC-10). Optional: nil disables
// binding (e.g. the container fallback, which has no vsock peer).
// sandboxd.PeerBinder satisfies it. Refs: FR-17.27, SEC-10
type PeerBinder interface {
	Bind(sandboxID, peerID string)
	Invalidate(sandboxID string)
}

// PeerIdentifier is implemented by a VM that knows its host-observed peer
// identity — the vsock CID on AF_VSOCK backends, the VM-GUID on Hyper-V.
// The identity is host-observed, never guest-asserted (SEC-05). A VM that
// reports none is bound under its sandbox ID, which is host-assigned and
// likewise unique per VM. Refs: SEC-10, SEC-05
type PeerIdentifier interface {
	PeerIdentity() string
}

// NotifyListenerProvider is implemented by a VM that exposes a host-side
// socket path the guest reaches the host on for the guest->host land-ready
// notification (the auto-land trigger). Firecracker's guest->host model has
// the host LISTEN on a per-VM "<vsock>_<port>" socket; this returns that path.
// A VM that returns "" has no guest->host notify path, so auto-land is not
// wired for it (the host-initiated `mgit sandbox land` is unaffected).
// Refs: MGIT-11.10.11, SEC-10
type NotifyListenerProvider interface {
	NotifySocketPath() string
}

// NotifyRegistrar starts and stops a sandbox's per-VM guest->host notify
// listener around its lifecycle: Register records the host-bound task and binds
// the per-VM listener at the host socket; Deregister tears it down at teardown
// (no residue, SEC-10). It is OPTIONAL (nil disables the auto-land trigger) and
// keyed by the host-observed identity so one guest can never trigger another's
// land. sandboxd.NotifyController satisfies it. Refs: MGIT-11.10.11, SEC-10
type NotifyRegistrar interface {
	Register(sandboxID, taskID, peerID, socketPath string) error
	Deregister(sandboxID string)
}

// Config wires the shared manager's dependencies.
type Config struct {
	Backend     string                                    // model.Backend* this platform reports
	WorkDir     string                                    // sandbox-local state root; never the worktree
	Resolve     func(imageRef string) (ImagePaths, error) // verified image resolution (FR-17.17)
	Hypervisor  Hypervisor
	GuestDialer GuestDialer // exec transport into the guest; nil = exec unavailable
	PeerBinder  PeerBinder  // channel peer-identity binder (SEC-10); nil disables
	// NotifyRegistrar starts/stops each sandbox's per-VM guest->host notify
	// listener (the auto-land trigger) across its lifecycle. Optional: nil
	// disables auto-land (the host-initiated land path is unaffected). Only used
	// when the VM exposes a notify socket path (NotifyListenerProvider).
	// Refs: MGIT-11.10.11, SEC-10
	NotifyRegistrar NotifyRegistrar
	// StoreProvisioner seeds the SEC-03 private, sandbox-local mgit store per
	// launch (from the task base commit only) and supplies the shared store
	// path for the non-reachability check. When set, the quarantine control is
	// REALIZED: every launch builds the plan, binds the private store, and
	// fails closed (ErrSharedStoreReachable) if the shared store could be
	// reached. When nil (legacy/direct path, tests) the worktree is delivered
	// without a private store rebind, the pre-SEC-03 behavior. Refs: SEC-03
	StoreProvisioner provision.Provisioner
	// SensitivePaths are the worktree-relative host-trusted patterns layered
	// read-only into the guest plan (FR-17.14). Only used when a provisioner
	// is wired. Refs: FR-17.14
	SensitivePaths []string
	// NetworkModeCheck reports whether this platform can ENFORCE a network
	// mode, so an unenforceable one is refused when the operator configures it
	// rather than when the VM is created minutes later (lazy provisioning,
	// FR-17.9/17.10). A backend MUST pass the same function its boot path
	// calls, so the two answers cannot drift. Nil accepts every mode — the
	// behavior of a backend that has not declared anything. Refs: MGIT-111, SEC-04
	NetworkModeCheck func(mode string) error
	Logger           *slog.Logger
	Clock            func() time.Time
	// GuestReadyTimeout bounds how long the first exec after a lazy launch
	// waits for the guest's control vsock to accept a connection. A launched
	// VM is StateRunning as soon as the VMM is up, but the guest userspace
	// (mgit-guest binding its vsock port) needs ~1s more to boot; without a
	// wait the first exec dials too early and the handshake resets (EOF).
	// Zero uses guestReadyTimeoutDefault. Refs: MGIT-58, FR-17.10, FR-17.11
	GuestReadyTimeout time.Duration
}

// sandbox is one supervised microVM.
type sandbox struct {
	info model.SandboxInfo
	vm   VM
	dir  string // per-sandbox state dir under WorkDir
	// privateStore is the host directory backing the guest's private store,
	// needed to re-stage the worktree for a sync (MGIT-71).
	privateStore string
	// syncMu serializes worktree sync against exec, so no command ever
	// observes a partially-applied tree. Refs: MGIT-71
	syncMu sync.Mutex
	// guestReady records that the guest has answered on the control vsock at
	// least once. Until it has, a command that gets nothing back is retried;
	// after it has, the same failure is real and is reported. Refs: MGIT-61.15
	guestReady bool
}

// Manager implements model.SandboxManager over a platform Hypervisor.
// The registry is in-memory by design: a microVM is a child resource
// of this process, so a daemon crash takes its VMs with it — there is
// no orphaned-VM state to recover, and the durable lifecycle record
// lives in the append-only sandbox_events table (FR-17.18).
type Manager struct {
	cfg Config

	mu        sync.Mutex
	sandboxes map[string]*sandbox
	entropy   *ulid.MonotonicEntropy
}

// NewManager validates the configuration and returns a Manager.
func NewManager(cfg Config) (*Manager, error) {
	switch {
	case cfg.Backend == "":
		return nil, fmt.Errorf("microvm: backend name must not be empty")
	case cfg.WorkDir == "":
		return nil, fmt.Errorf("microvm: work dir must not be empty")
	case cfg.Resolve == nil:
		return nil, fmt.Errorf("microvm: image resolver must not be nil")
	case cfg.Hypervisor == nil:
		return nil, fmt.Errorf("microvm: hypervisor must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("microvm: logger must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("microvm: clock must not be nil")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("microvm: create work dir: %w", err)
	}
	if cfg.GuestReadyTimeout <= 0 {
		cfg.GuestReadyTimeout = guestReadyTimeoutDefault
	}
	return &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
		entropy:   ulid.Monotonic(rand.Reader, 0),
	}, nil
}

// guestReadyTimeoutDefault bounds the first-exec wait for the guest vsock
// listener after a lazy launch. Generous headroom over the ~1s guest boot,
// so a genuinely dead guest still fails in bounded time. Refs: MGIT-58
const guestReadyTimeoutDefault = 15 * time.Second

// guestReadyPollInterval is the backoff between guest vsock dial attempts
// while the guest boots. Small enough that exec latency tracks actual guest
// readiness, not the poll grain. Refs: MGIT-58
const guestReadyPollInterval = 50 * time.Millisecond

// Launch boots one microVM bound to the task's worktree and registers
// it before returning (the FR-17.26 ceiling depends on that ordering).
func (m *Manager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
	}
	images, err := m.cfg.Resolve(opts.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("%s launch: resolve image %q: %w", m.cfg.Backend, opts.ImageRef, err)
	}

	// Use the host-assigned lifecycle ID when the caller (the sandbox
	// service, lazy provisioning) supplied one, so registration and boot
	// share one ID; otherwise generate (direct/legacy use). Refs: FR-17.10
	id := opts.SandboxID
	if id == "" {
		var err error
		if id, err = m.newID(); err != nil {
			return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
		}
	}
	dir := SandboxStateDir(m.cfg.WorkDir, id)
	overlay, err := createOverlay(dir, opts.DiskQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
	}

	// SEC-03: provision the private, sandbox-local store and prove the host
	// shared store is unreachable from the guest plan BEFORE the VM exists. A
	// quarantine failure fails the launch closed (ErrSharedStoreReachable) —
	// the guest never boots against a leaky layout. Refs: SEC-03, FR-17.3
	privateStore, err := m.quarantine(opts.TaskID, opts.WorktreePath, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
	}

	vm, err := m.cfg.Hypervisor.CreateVM(vmConfig(id, dir, opts, images, overlay, privateStore))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s launch: create vm: %w", m.cfg.Backend, err)
	}
	if err := vm.Start(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s launch: start vm: %w", m.cfg.Backend, err)
	}

	info := m.newSandboxInfo(id, opts)

	// Record what was DELIVERED, so a later sync can tell a host edit from a
	// guest one. Backends that stage into a shared host directory (virtiofs)
	// can then propagate host changes into a running guest; backends that
	// deliver a block image built at launch cannot, and simply have no staged
	// tree here. Refs: MGIT-71, ADR-011
	if staged := stagedTreePath(dir); staged != "" {
		if err := worktreesync.RecordDelivery(staged, dir); err != nil {
			m.cfg.Logger.Warn("could not record the worktree delivery baseline; "+
				"sync will treat every host path as new",
				"event", "sync_baseline_failed", "sandbox_id", id, "error", err.Error())
		}
	}

	m.mu.Lock()
	m.sandboxes[id] = &sandbox{info: info, vm: vm, dir: dir, privateStore: privateStore}
	m.mu.Unlock()

	// Bind the sandbox to its host-observed peer identity so incoming
	// guest->host channels can be authorized against it (SEC-10). The VM
	// reports the identity when it knows one (the vsock CID); otherwise the
	// host-assigned sandbox ID is used. Refs: FR-17.27, SEC-10
	if m.cfg.PeerBinder != nil {
		m.cfg.PeerBinder.Bind(id, peerIdentity(vm, id))
	}

	// Start the per-VM guest->host notify listener (the auto-land trigger) when
	// a registrar is wired and the VM exposes a notify socket. A failure here is
	// non-fatal: the sandbox runs and the host-initiated land still works; only
	// auto-land is unavailable. Authorization keys on the same host-observed
	// peer identity the binder recorded (SEC-10). Refs: MGIT-11.10.11, SEC-10
	m.registerNotify(vm, id, opts.TaskID)

	// FAIL CLOSED: confirm the guest is actually serving before calling this a
	// launch. Everything above proves only that the VMM started. Refs: MGIT-92
	if err := m.confirmGuestServing(ctx, id, vm); err != nil {
		return nil, err
	}

	m.cfg.Logger.Info("sandbox launched", "event", "launched", "backend", m.cfg.Backend,
		"sandbox_id", id, "task_id", opts.TaskID, "network_mode", opts.Network.Mode)
	return &info, nil
}

// registerNotify starts a sandbox's per-VM guest->host notify listener when a
// registrar is wired and the VM exposes a notify socket path. Best-effort: a
// listen failure is logged and leaves only the auto-land trigger disabled.
// Refs: MGIT-11.10.11, SEC-10
func (m *Manager) registerNotify(vm VM, sandboxID, taskID string) {
	if m.cfg.NotifyRegistrar == nil {
		return
	}
	provider, ok := vm.(NotifyListenerProvider)
	if !ok {
		return
	}
	socketPath := provider.NotifySocketPath()
	if socketPath == "" {
		return
	}
	if err := m.cfg.NotifyRegistrar.Register(sandboxID, taskID, peerIdentity(vm, sandboxID), socketPath); err != nil {
		m.cfg.Logger.Warn("sandbox auto-land notify listener not started",
			"event", "notify_unwired", "backend", m.cfg.Backend, "sandbox_id", sandboxID, "error", err.Error())
	}
}

// privateStoreDirName is the per-sandbox private store directory under the
// state dir — OUTSIDE the worktree, sandbox-local (cleaned by teardown's one
// RemoveAll). It is never the worktree and never the shared store. Refs: SEC-03
const privateStoreDirName = "private-store"

// quarantine realizes the SEC-03 control for one launch: when a store
// provisioner is wired, it seeds a fresh private store (task base commit only)
// under the sandbox state dir, builds the guest filesystem plan, binds the
// private store, and rejects the launch if the host shared store could be
// reached (ErrSharedStoreReachable). It returns the private store host path the
// backend delivers at the guest's .mgit. When no provisioner is wired it is a
// no-op (empty path) — the pre-SEC-03 delivery, used by tests and the direct
// path. Refs: SEC-03, FR-17.3, FR-17.5, FR-17.14
func (m *Manager) quarantine(taskID, worktreePath, stateDir string) (string, error) {
	if m.cfg.StoreProvisioner == nil {
		return "", nil // quarantine not wired (legacy/direct path)
	}
	privDir := filepath.Join(stateDir, privateStoreDirName)
	store, err := m.cfg.StoreProvisioner.Provision(taskID, privDir)
	if err != nil {
		return "", fmt.Errorf("provision private store: %w", err)
	}
	plan, err := quarantine.BuildPlan(worktreePath, m.cfg.SensitivePaths)
	if err != nil {
		return "", fmt.Errorf("build quarantine plan: %w", err)
	}
	// BindPrivateStore enforces the SEC-03 invariants and returns
	// ErrSharedStoreReachable if the shared store is reachable; the caller
	// rejects the launch, so a leaky layout never boots a guest.
	if _, err := plan.BindPrivateStore(store.Dir, store.SharedDir); err != nil {
		return "", fmt.Errorf("bind private store: %w", err)
	}
	return store.Dir, nil
}

// vmConfig builds the hypervisor-agnostic VM description carrying the
// FR-17 isolation contract: read-only pinned rootfs + per-VM COW
// overlay (FR-17.17), worktree share, the SEC-03 private store, vsock
// control plane, and a NIC only when the network mode is not "none"
// (FR-17.7). Refs: FR-17.3, FR-17.17, SEC-03
func vmConfig(id, stateDir string, opts model.SandboxLaunchOptions, images ImagePaths, overlay, privateStore string) VMConfig {
	return VMConfig{
		SandboxID:        id,
		StateDir:         stateDir,
		TaskID:           opts.TaskID,
		CPUs:             opts.CPUs,
		MemoryMB:         opts.MemoryMB,
		KernelPath:       images.KernelPath,
		RootfsPath:       images.RootfsPath,
		RootfsReadOnly:   true,
		Cmdline:          images.Cmdline,
		OverlayPath:      overlay,
		WorktreePath:     opts.WorktreePath,
		PrivateStorePath: privateStore,
		WorktreeTag:      "work",
		AttachNIC:        opts.Network.Mode != model.NetworkModeNone,
		NetworkMode:      opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		VsockEnabled:     true,
		BalloonEnabled:   true,
		PublishPorts:     guestPublishPorts(opts.PublishPorts),
	}
}

// guestPublishPorts projects the launch options' host->guest port mappings to
// just the GUEST ports the guest must bridge (the host port is host-side
// state the guest never learns — SEC-09 keeps the cmdline free of host
// addresses). Refs: SEC-09, FR-17.8
func guestPublishPorts(ports []model.PortPublish) []int {
	if len(ports) == 0 {
		return nil
	}
	out := make([]int, 0, len(ports))
	for _, pp := range ports {
		out = append(out, pp.GuestPort)
	}
	return out
}

// newSandboxInfo assembles the running sandbox's metadata record,
// stamping the host clock (and TTL expiry when set).
func (m *Manager) newSandboxInfo(id string, opts model.SandboxLaunchOptions) model.SandboxInfo {
	now := m.cfg.Clock().UTC()
	info := model.SandboxInfo{
		ID:               id,
		TaskID:           opts.TaskID,
		WorktreePath:     opts.WorktreePath,
		Backend:          m.cfg.Backend,
		ImageDigest:      imageDigest(opts.ImageRef),
		NetworkMode:      opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		State:            model.StateRunning,
		MemoryMB:         opts.MemoryMB,
		CreatedAt:        now,
		// The launch options' port mappings, so Status/published (SEC-09)
		// can report what was actually configured — guestPublishPorts above
		// only projects the guest-side port numbers into the VM config;
		// nothing previously carried the full HostPort/GuestPort pairs into
		// the record Resolve/List return. Refs: SEC-09, MGIT-61.13
		PublishPorts: opts.PublishPorts,
	}
	if opts.TTL > 0 {
		info.ExpiresAt = now.Add(opts.TTL)
	}
	return info
}

// SupportsNetworkMode reports whether this backend can ENFORCE mode,
// satisfying model.NetworkModeEnforcer so the service can refuse an
// unenforceable mode at registration. Refs: MGIT-111, SEC-04
func (m *Manager) SupportsNetworkMode(mode string) error {
	if m.cfg.NetworkModeCheck == nil {
		return nil
	}
	return m.cfg.NetworkModeCheck(mode)
}

// List returns every supervised sandbox.
func (m *Manager) List(_ context.Context) ([]model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.SandboxInfo, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb.info)
	}
	return out, nil
}

// Exec routes one whole command into the running guest over the exec
// channel and returns its buffered output and exit code. The command is
// sent verbatim (whole-command routing, FR-17.11) and the host
// environment is never forwarded — only req.Env reaches the guest. A
// non-zero exit is a normal result, not an error. When no guest dialer is
// configured the transport is honestly reported unavailable rather than
// faked. Refs: FR-17.11, FR-17.3
func (m *Manager) Exec(ctx context.Context, id string, req model.ExecRequest) (*model.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%s exec: %w", m.cfg.Backend, err)
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State != model.StateRunning {
		return nil, fmt.Errorf("%w: sandbox %q is %s, not running",
			model.ErrSandboxBackendUnavailable, id, sb.info.State)
	}
	// Carry host worktree changes in BEFORE the command runs, so the agent
	// loop tests the code the host actually has rather than a launch-time
	// snapshot. Unchanged host worktree => no work; a conflict with un-landed
	// guest work refuses the exec by name. Refs: MGIT-71, ADR-011
	if err := m.syncBeforeExec(ctx, sb); err != nil {
		return nil, err
	}
	if m.cfg.GuestDialer == nil {
		return nil, fmt.Errorf("%w: exec requires the guest vsock transport (MGIT-11.9.2)",
			model.ErrSandboxBackendUnavailable)
	}

	return m.execUntilTheGuestAnswers(ctx, id, req)
}

// execUntilTheGuestAnswers runs one command, retrying the whole exchange
// while the guest has never yet answered on this sandbox.
//
// A dial that succeeds is not proof the guest is up. libkrun's host-side
// vsock endpoint exists from the moment the VM starts, so during the ~1s the
// guest takes to boot, the connection is accepted, the request is written,
// and the connection is then closed with nothing forwarded — which reached
// the user as a bare "read frame: EOF" on the FIRST command after
// `mgit work --sandbox`, with the second command working. That is the worst
// possible shape for trust in the tool.
//
// The retry is narrow on purpose. It applies only while this sandbox's guest
// has never once answered, and only when the failure was an EOF with nothing
// received — no output, no result frame. Under those two conditions the
// command provably never reached a listener, so re-sending it cannot run
// anything twice. Once the guest has answered, a dropped connection means
// something went wrong mid-command and is reported as it happens.
// Refs: MGIT-61.15, MGIT-58, FR-17.11
func (m *Manager) execUntilTheGuestAnswers(
	ctx context.Context, id string, req model.ExecRequest,
) (*model.ExecResult, error) {
	deadline := time.Now().Add(m.cfg.GuestReadyTimeout)
	for attempt := 0; ; attempt++ {
		result, stdout, stderr, err := m.execOnce(ctx, id, req)
		if err == nil {
			m.markGuestAnswered(id)
			return &model.ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: result.ExitCode}, nil
		}
		if !m.guestNeverAnswered(id) || !isSilentDisconnect(err, stdout, stderr) ||
			!time.Now().Add(guestReadyPollInterval).Before(deadline) {
			return nil, fmt.Errorf("%s exec: %w", m.cfg.Backend, err)
		}
		m.cfg.Logger.Debug("guest not serving yet; retrying the first command",
			"event", "guest_not_serving", "sandbox_id", id, "attempts", attempt+1)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s exec: %w", m.cfg.Backend, err)
		case <-time.After(guestReadyPollInterval):
		}
	}
}

// execOnce performs a single dial-and-run exchange.
func (m *Manager) execOnce(
	ctx context.Context, id string, req model.ExecRequest,
) (execwire.Result, []byte, []byte, error) {
	conn, err := m.dialGuestReady(ctx, id)
	if err != nil {
		return execwire.Result{}, nil, nil, fmt.Errorf("dial guest: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var stdout, stderr bytes.Buffer
	result, err := guestexec.Run(conn, req, &stdout, &stderr)
	return result, stdout.Bytes(), stderr.Bytes(), err
}

// isSilentDisconnect reports whether the guest dropped the connection having
// sent NOTHING at all — the signature of a command that never reached a
// listener, and therefore one that can be re-sent without running anything
// twice.
//
// IT MUST MATCH THE RESET, not just the clean EOF, and that omission was the
// whole of MGIT-91. libkrun creates the host-side vsock socket as soon as the
// VM is configured, so a dial in the window before mgit-guest binds its
// listener CONNECTS successfully and is then reset by the VMM: the caller sees
// ECONNRESET, never io.EOF. Matching only io.EOF meant the first-command retry
// this function gates never fired for that backend — measured on real KVM,
// where the guest's console proves it logged nothing at all for the failed
// attempt and served the next one normally.
//
// EPIPE is the same event seen from the writing side (the request was reset
// before it was fully sent), so it is included on the same reasoning: with no
// output at all, nothing can have run.
//
// THE SAFETY ARGUMENT IS THE CALLER'S THREE GUARDS, not this predicate: a
// retry happens only while the guest has NEVER answered, only with no output
// whatsoever, and only inside the readiness deadline. Once the guest has
// answered once, a reset means a real failure mid-command — an agent whose
// long build dies mid-stream must see that, not have it silently retried.
// Refs: MGIT-91, MGIT-58, MGIT-61.15, FR-17.11
func isSilentDisconnect(err error, stdout, stderr []byte) bool {
	if len(stdout) != 0 || len(stderr) != 0 {
		return false
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// guestNeverAnswered reports whether this sandbox's guest has yet to answer
// on the control channel.
func (m *Manager) guestNeverAnswered(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	return ok && !sb.guestReady
}

// markGuestAnswered records that the guest served a command, which ends the
// first-command retry window for this sandbox.
func (m *Manager) markGuestAnswered(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		sb.guestReady = true
	}
}

// guestProbeCommand is the readiness probe's argv. It names a program that
// deliberately DOES NOT EXIST in any guest, because the probe's purpose is to
// get an ANSWER, not to run anything: the guest resolves it, fails the lookup,
// and replies on the wire. That reply is the proof we want — the control plane
// is bound and serving — and it costs the guest no process and no side effect,
// on any image, including one that ships its own guest binary. A reader who
// finds it in a console log can tell what it is from its name.
// Refs: MGIT-92, FR-17.11
var guestProbeCommand = []string{"mgit-guest-readiness-probe"}

// consoleTailBytes bounds how much guest console a failed launch quotes. Large
// enough for a Go panic with a few frames, small enough that an agent's error
// stays readable.
const consoleTailBytes = 4 << 10

// awaitGuestServing blocks until the guest ANSWERS on its control channel, or
// the readiness deadline passes.
//
// A DIAL IS NOT ENOUGH, and that is the whole subtlety: libkrun creates the
// host-side vsock socket when the VM is configured, so a connect succeeds long
// before mgit-guest binds and the failure only shows up as a reset on the first
// read (MGIT-91). Proving the guest is there therefore requires a round trip,
// so this sends the probe request and accepts ANY well-formed reply — including
// the guest refusing the probe, which is the expected reply and still proves
// the channel serves. Only a SILENT disconnect means "not there yet", using the
// same predicate and the same GuestReadyTimeout as the first-command retry
// rather than a second notion of readiness.
//
// It deliberately does NOT mark the guest answered. A probe the guest rejects
// at lookup is slightly weaker evidence than a real command round trip, and
// leaving the first-command retry window OPEN costs nothing (it only ever fires
// on a silent disconnect with no output) while keeping MGIT-91's protection
// reachable for the caller's first real command.
// Refs: MGIT-92, MGIT-91, FR-17.11, NFR-17.6
func (m *Manager) awaitGuestServing(ctx context.Context, id string) error {
	req := model.ExecRequest{Command: guestProbeCommand}
	deadline := time.Now().Add(m.cfg.GuestReadyTimeout)
	var lastErr error
	for attempt := 0; ; attempt++ {
		_, _, _, err := m.execOnce(ctx, id, req)
		// READY means the GUEST answered: either it ran the probe (it will not)
		// or, as expected, it replied refusing to. Anything else — a dial that
		// never reaches a listener, a silent disconnect, a mangled frame — is
		// not an answer. Getting this backwards is easy and was caught only on
		// real hardware: a dead guest's dial failure is not a silent
		// disconnect, so treating "not a silent disconnect" as proof of life
		// declared a launched sandbox over a guest that had already exited.
		if err == nil || errors.Is(err, guestexec.ErrGuestReported) {
			return nil
		}
		lastErr = err
		if !time.Now().Add(guestReadyPollInterval).Before(deadline) {
			return fmt.Errorf("%w within %s: %w",
				model.ErrGuestNotServing, m.cfg.GuestReadyTimeout, lastErr)
		}
		m.cfg.Logger.Debug("guest not serving yet; still waiting out its boot",
			"event", "guest_boot_wait", "sandbox_id", id, "attempts", attempt+1)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", model.ErrGuestNotServing, ctx.Err())
		case <-time.After(guestReadyPollInterval):
		}
	}
}

// confirmGuestServing makes a launch FAIL CLOSED when the guest never comes up:
// it waits for the guest to answer and, on timeout, tears the sandbox down and
// returns the guest's own console tail as the diagnosis.
//
// Reporting a sandbox that no exec can use is the failure this exists to
// prevent — it is the same contract `mgit run` already honors by refusing to
// fall back to the host (NFR-17.6), enforced one step earlier, at the step the
// operator actually reads. The ~1s this adds to a boot is the point, not a
// cost: an agent walking into a sandbox that does not exist costs far more.
//
// It is skipped when no GuestDialer is wired, because then there is no control
// plane to confirm and Exec already reports the transport unavailable rather
// than faking success. That keeps the wait expressed ONCE, in the manager,
// while still only asserting a capability the backend actually has.
// Refs: MGIT-92, NFR-17.6, FR-17.11
func (m *Manager) confirmGuestServing(ctx context.Context, id string, vm VM) error {
	if m.cfg.GuestDialer == nil {
		return nil
	}
	err := m.awaitGuestServing(ctx, id)
	if err == nil {
		return nil
	}
	// Read the console BEFORE teardown: Remove deletes the state dir, and the
	// log inside it is the only place the guest's own error exists.
	detail := consoleDiagnosis(vm)
	m.cfg.Logger.Error("launch failed closed: the guest never answered",
		"event", "launch_guest_not_serving", "backend", m.cfg.Backend,
		"sandbox_id", id, "error", err.Error())
	return errors.Join(
		fmt.Errorf("%s launch: %w\n%s", m.cfg.Backend, err, detail),
		m.Stop(ctx, id, true),
		m.Remove(ctx, id, true),
	)
}

// consoleDiagnosis renders the guest console tail for a failed launch, or says
// plainly that this backend captured none — never an empty string, because a
// blank space where the diagnosis should be reads like a missing feature.
// Refs: MGIT-92
func consoleDiagnosis(vm VM) string {
	tailer, ok := vm.(ConsoleTailer)
	if !ok {
		return "guest console: not captured by this backend"
	}
	tail := strings.TrimSpace(tailer.ConsoleTail(consoleTailBytes))
	if tail == "" {
		// Phase-correct wording: this function is only ever reached from a
		// launch that failed closed, i.e. a guest that never answered at all.
		// Saying it "stopped answering" described a serving guest that died,
		// which is a different failure with a different fix (MGIT-104).
		return "guest console: empty (the guest never wrote anything)"
	}
	return "guest console (tail):\n" + tail
}

// TailFile returns at most maxBytes from the END of the file at path, or ""
// when it cannot be read. Backends implementing ConsoleTailer use it so the
// per-backend method stays one line and the bounding rule stays in one place.
// Refs: MGIT-92
func TailFile(path string, maxBytes int) string {
	f, err := os.Open(path) //nolint:gosec // manager-owned per-sandbox state dir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size > int64(maxBytes) {
		if _, err := f.Seek(size-int64(maxBytes), io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// dialGuestReady dials the guest control vsock, retrying on failure until
// the guest's listener is up or GuestReadyTimeout elapses. The sandbox is
// already StateRunning (the VMM is up), so a dial failure here means the
// guest userspace has not finished booting and bound its vsock port yet —
// a transient, retryable "not ready" condition, not a permanent fault.
// Without this, the first exec after a lazy launch (FR-17.10) races the
// ~1s guest boot and the firecracker/vz handshake resets with EOF (MGIT-58).
// Backend-agnostic: both the firecracker and vzf dialers benefit. The
// caller's ctx still bounds and cancels the wait; the readiness timeout is
// the upper bound when ctx has none. Refs: MGIT-58, FR-17.10, FR-17.11
func (m *Manager) dialGuestReady(ctx context.Context, id string) (net.Conn, error) {
	deadline := time.Now().Add(m.cfg.GuestReadyTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d // never wait past the caller's own deadline
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("guest not ready: %w", lastErr)
			}
			return nil, err
		}
		conn, err := m.cfg.GuestDialer.DialGuest(ctx, id)
		if err == nil {
			if attempt > 0 {
				m.cfg.Logger.Debug("guest vsock ready after retry",
					"event", "guest_ready", "sandbox_id", id, "attempts", attempt+1)
			}
			return conn, nil
		}
		lastErr = err
		if !time.Now().Add(guestReadyPollInterval).Before(deadline) {
			return nil, fmt.Errorf("guest vsock not ready within %s: %w", m.cfg.GuestReadyTimeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("guest not ready: %w", lastErr)
		case <-time.After(guestReadyPollInterval):
		}
	}
}

// Stop halts the sandbox's VM and records it suspended. v1 does not
// resume a stopped VM; the NFR-17.3 idle-suspend/resume cycle arrives
// with the lifecycle service (MGIT-11.9.5).
func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if err := sb.vm.Stop(ctx, force); err != nil {
		return fmt.Errorf("%s stop: %w", m.cfg.Backend, err)
	}
	m.mu.Lock()
	sb.info.State = model.StateSuspended
	m.mu.Unlock()
	return nil
}

// Remove tears the sandbox down: VM stopped, every sandbox-local file
// deleted, registration dropped. The worktree is never touched
// (FR-17.19).
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State == model.StateRunning {
		if err := sb.vm.Stop(ctx, force); err != nil && !force {
			return fmt.Errorf("%s remove: stop: %w", m.cfg.Backend, err)
		}
	}
	if err := os.RemoveAll(sb.dir); err != nil {
		return fmt.Errorf("%s remove: clear sandbox dir: %w", m.cfg.Backend, err)
	}

	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Drop the peer binding so a connection still addressing this sandbox —
	// or a recycled CID handed to a successor VM — cannot reach the
	// destroyed channel (SEC-10). Refs: FR-17.27
	if m.cfg.PeerBinder != nil {
		m.cfg.PeerBinder.Invalidate(id)
	}

	// Stop the per-VM notify listener so a torn-down sandbox can no longer
	// auto-land and a recycled identity cannot inherit its trigger (SEC-10,
	// FR-17.19). Refs: MGIT-11.10.11
	if m.cfg.NotifyRegistrar != nil {
		m.cfg.NotifyRegistrar.Deregister(id)
	}

	m.cfg.Logger.Info("sandbox removed", "event", "removed", "backend", m.cfg.Backend, "sandbox_id", id)
	return nil
}

// peerIdentity returns the VM's host-observed peer identity, falling back
// to the (host-assigned, unique) sandbox ID when the VM reports none.
func peerIdentity(vm VM, sandboxID string) string {
	if pi, ok := vm.(PeerIdentifier); ok {
		return pi.PeerIdentity()
	}
	return sandboxID
}

// Resolve returns one sandbox by id.
func (m *Manager) Resolve(_ context.Context, id string) (*model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		info := sb.info
		return &info, nil
	}
	return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
}

// newID returns a monotonically increasing ULID.
func (m *Manager) newID() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := ulid.New(ulid.Timestamp(m.cfg.Clock().UTC()), m.entropy)
	if err != nil {
		return "", fmt.Errorf("new ulid: %w", err)
	}
	return id.String(), nil
}

// stateDirSegmentLen is how much of the sandbox ID names its state
// directory. Refs: MGIT-61.15
const stateDirSegmentLen = 8

// SandboxStateDir returns the per-sandbox state directory: a subdirectory of
// the manager's work dir named from the sandbox ID. It holds every
// per-sandbox host artifact (the COW overlay and the backend's sockets), so
// teardown is one RemoveAll. It is exported as the single source of this
// convention: a backend's guest dialer reconstructs a sandbox's socket path
// from the same dir, so both must agree. Refs: FR-17.19
//
// It uses the TAIL of the ID rather than the whole thing because unix sockets
// are bound under this directory and sun_path caps the entire path at 104
// bytes. macOS hands every process a 48-byte private TMPDIR, which is where
// the daemon's runtime dir lands; a full 26-character ULID on top of that put
// every socket path over the limit, so no VM could boot on a stock Mac at
// all. The tail is used, not the head: a ULID's leading characters are its
// timestamp and are identical for sandboxes created in the same millisecond,
// while the tail is the random part. It remains a suffix of the ID, so a
// directory found on disk still greps back to its sandbox.
func SandboxStateDir(workDir, sandboxID string) string {
	seg := sandboxID
	if len(seg) > stateDirSegmentLen {
		seg = seg[len(seg)-stateDirSegmentLen:]
	}
	return filepath.Join(workDir, seg)
}

// createOverlay creates the per-sandbox writable disk as a SPARSE file
// of the quota size under the sandbox state dir (0700; never inside
// the worktree) — sparse, so disk is consumed only by what the task
// writes (NFR-17.7). Refs: FR-17.17, NFR-17.5
func createOverlay(dir string, sizeMB int) (string, error) {
	if sizeMB <= 0 {
		sizeMB = defaultOverlaySizeMB
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create sandbox dir: %w", err)
	}
	overlay := filepath.Join(dir, "overlay.img")
	file, err := os.OpenFile(overlay, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path built from manager-owned dir
	if err != nil {
		return "", fmt.Errorf("create overlay: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Truncate(int64(sizeMB) << 20); err != nil {
		return "", fmt.Errorf("size overlay: %w", err)
	}
	return overlay, nil
}

// imageDigest extracts the sha256:<hex> digest from a pinned reference
// (already validated by SandboxLaunchOptions.Validate).
func imageDigest(imageRef string) string {
	_, digest, found := strings.Cut(imageRef, "@")
	if !found {
		return ""
	}
	return digest
}
