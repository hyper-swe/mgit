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
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
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
	SandboxID      string // host-assigned lifecycle ID; lets a backend key a live-VM registry to its dialer (vzf, FR-17.16)
	CPUs           int
	MemoryMB       int
	KernelPath     string
	RootfsPath     string
	RootfsReadOnly bool // the pinned image is immutable (FR-17.17)
	Cmdline        string
	OverlayPath    string // per-sandbox COW backing file (FR-17.17), pre-sized to the quota
	WorktreePath   string // shared at the identical guest path (FR-17.3)
	WorktreeTag    string // mount tag
	AttachNIC      bool   // false in none mode (FR-17.7)
	VsockEnabled   bool
	BalloonEnabled bool
}

// VM is the lifecycle handle the manager drives.
type VM interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, force bool) error
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

// Config wires the shared manager's dependencies.
type Config struct {
	Backend     string                                    // model.Backend* this platform reports
	WorkDir     string                                    // sandbox-local state root; never the worktree
	Resolve     func(imageRef string) (ImagePaths, error) // verified image resolution (FR-17.17)
	Hypervisor  Hypervisor
	GuestDialer GuestDialer // exec transport into the guest; nil = exec unavailable
	PeerBinder  PeerBinder  // channel peer-identity binder (SEC-10); nil disables
	Logger      *slog.Logger
	Clock       func() time.Time
}

// sandbox is one supervised microVM.
type sandbox struct {
	info model.SandboxInfo
	vm   VM
	dir  string // per-sandbox state dir under WorkDir
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
	return &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
		entropy:   ulid.Monotonic(rand.Reader, 0),
	}, nil
}

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

	vm, err := m.cfg.Hypervisor.CreateVM(vmConfig(id, opts, images, overlay))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s launch: create vm: %w", m.cfg.Backend, err)
	}
	if err := vm.Start(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%s launch: start vm: %w", m.cfg.Backend, err)
	}

	info := m.newSandboxInfo(id, opts)

	m.mu.Lock()
	m.sandboxes[id] = &sandbox{info: info, vm: vm, dir: dir}
	m.mu.Unlock()

	// Bind the sandbox to its host-observed peer identity so incoming
	// guest->host channels can be authorized against it (SEC-10). The VM
	// reports the identity when it knows one (the vsock CID); otherwise the
	// host-assigned sandbox ID is used. Refs: FR-17.27, SEC-10
	if m.cfg.PeerBinder != nil {
		m.cfg.PeerBinder.Bind(id, peerIdentity(vm, id))
	}

	m.cfg.Logger.Info("sandbox launched", "event", "launched", "backend", m.cfg.Backend,
		"sandbox_id", id, "task_id", opts.TaskID, "network_mode", opts.Network.Mode)
	return &info, nil
}

// vmConfig builds the hypervisor-agnostic VM description carrying the
// FR-17 isolation contract: read-only pinned rootfs + per-VM COW
// overlay (FR-17.17), worktree share, vsock control plane, and a NIC
// only when the network mode is not "none" (FR-17.7).
func vmConfig(id string, opts model.SandboxLaunchOptions, images ImagePaths, overlay string) VMConfig {
	return VMConfig{
		SandboxID:      id,
		CPUs:           opts.CPUs,
		MemoryMB:       opts.MemoryMB,
		KernelPath:     images.KernelPath,
		RootfsPath:     images.RootfsPath,
		RootfsReadOnly: true,
		Cmdline:        images.Cmdline,
		OverlayPath:    overlay,
		WorktreePath:   opts.WorktreePath,
		WorktreeTag:    "work",
		AttachNIC:      opts.Network.Mode != model.NetworkModeNone,
		VsockEnabled:   true,
		BalloonEnabled: true,
	}
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
	}
	if opts.TTL > 0 {
		info.ExpiresAt = now.Add(opts.TTL)
	}
	return info
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
	if m.cfg.GuestDialer == nil {
		return nil, fmt.Errorf("%w: exec requires the guest vsock transport (MGIT-11.9.2)",
			model.ErrSandboxBackendUnavailable)
	}

	conn, err := m.cfg.GuestDialer.DialGuest(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%s exec: dial guest: %w", m.cfg.Backend, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var stdout, stderr bytes.Buffer
	result, err := guestexec.Run(conn, req, &stdout, &stderr)
	if err != nil {
		return nil, fmt.Errorf("%s exec: %w", m.cfg.Backend, err)
	}
	return &model.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: result.ExitCode}, nil
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

// SandboxStateDir returns the per-sandbox state directory: a subdirectory
// of the manager's work dir named for the sandbox ID. It holds every
// per-sandbox host artifact (the COW overlay and the backend's sockets),
// so teardown is one RemoveAll. It is exported as the single source of
// this convention: a backend's guest dialer reconstructs a sandbox's
// socket path from the same dir, so both must agree. Refs: FR-17.19
func SandboxStateDir(workDir, sandboxID string) string {
	return filepath.Join(workDir, sandboxID)
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
