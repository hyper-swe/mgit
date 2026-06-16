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
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/model"
)

// defaultOverlaySizeMB sizes the writable overlay when the request
// leaves the disk quota unresolved (NFR-17.5 default).
const defaultOverlaySizeMB = 4096

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

// Config wires the shared manager's dependencies.
type Config struct {
	Backend    string                                    // model.Backend* this platform reports
	WorkDir    string                                    // sandbox-local state root; never the worktree
	Resolve    func(imageRef string) (ImagePaths, error) // verified image resolution (FR-17.17)
	Hypervisor Hypervisor
	Logger     *slog.Logger
	Clock      func() time.Time
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

	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
	}
	dir := filepath.Join(m.cfg.WorkDir, id)
	overlay, err := createOverlay(dir, opts.DiskQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("%s launch: %w", m.cfg.Backend, err)
	}

	vm, err := m.cfg.Hypervisor.CreateVM(vmConfig(opts, images, overlay))
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

	m.cfg.Logger.Info("sandbox launched", "event", "launched", "backend", m.cfg.Backend,
		"sandbox_id", id, "task_id", opts.TaskID, "network_mode", opts.Network.Mode)
	return &info, nil
}

// vmConfig builds the hypervisor-agnostic VM description carrying the
// FR-17 isolation contract: read-only pinned rootfs + per-VM COW
// overlay (FR-17.17), worktree share, vsock control plane, and a NIC
// only when the network mode is not "none" (FR-17.7).
func vmConfig(opts model.SandboxLaunchOptions, images ImagePaths, overlay string) VMConfig {
	return VMConfig{
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

// Exec routes one command into the guest. The vsock exec transport is
// the mgit-guest agent's contract (MGIT-11.5.6); until the host wires
// it, exec reports honestly that the transport is unavailable rather
// than faking success. Refs: FR-17.11
func (m *Manager) Exec(_ context.Context, id string, _ model.ExecRequest) (*model.ExecResult, error) {
	m.mu.Lock()
	_, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	return nil, fmt.Errorf("%w: exec requires the mgit-guest vsock transport (MGIT-11.5.6)",
		model.ErrSandboxBackendUnavailable)
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

	m.cfg.Logger.Info("sandbox removed", "event", "removed", "backend", m.cfg.Backend, "sandbox_id", id)
	return nil
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
