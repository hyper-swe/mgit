// Package vzf is the macOS Virtualization.framework sandbox backend
// (FR-17.15): one microVM per task-bound worktree, read-only pinned
// rootfs with a per-sandbox COW overlay, the worktree shared at the
// identical path, vsock control plane, and memory ballooning. All
// vz/CGO calls live behind the hypervisor seam in the darwin-tagged
// file, so this package's logic stays portable and unit-testable and
// the ADR-005 CGO containment holds. Refs: FR-17.3, FR-17.15, FR-17.16
package vzf

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

// ImagePaths locates one resolved, digest-pinned guest image.
// Resolution from an image reference happens against the host-side
// images.lock (FR-17.17, MGIT-11.5.5).
type ImagePaths struct {
	KernelPath string // guest kernel
	RootfsPath string // read-only root filesystem image
	Cmdline    string // kernel command line
}

// vmConfig is the hypervisor-agnostic VM description the manager
// builds; the darwin implementation translates it to vz configuration.
type vmConfig struct {
	CPUs           int
	MemoryMB       int
	KernelPath     string
	RootfsPath     string
	RootfsReadOnly bool
	Cmdline        string
	OverlayPath    string // per-sandbox COW backing file (FR-17.17)
	WorktreePath   string // shared at the identical guest path (FR-17.3)
	WorktreeTag    string // virtiofs mount tag
	AttachNIC      bool   // false in none mode (FR-17.7)
	VsockEnabled   bool
	BalloonEnabled bool
}

// virtualMachine is the lifecycle handle the manager drives.
type virtualMachine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context, force bool) error
}

// hypervisor creates VMs from configs. Implemented by the vz bindings
// on darwin (hypervisor_darwin.go) and by fakes in tests.
type hypervisor interface {
	CreateVM(cfg vmConfig) (virtualMachine, error)
}

// Config wires the manager's dependencies.
type Config struct {
	WorkDir    string                                    // sandbox-local state root (overlays); never the worktree
	Resolve    func(imageRef string) (ImagePaths, error) // images.lock resolution (FR-17.17)
	Hypervisor hypervisor                                // nil selects the platform hypervisor
	Logger     *slog.Logger
	Clock      func() time.Time
}

// sandbox is one supervised microVM.
type sandbox struct {
	info model.SandboxInfo
	vm   virtualMachine
	dir  string // per-sandbox state dir under WorkDir
}

// Manager implements model.SandboxManager on Virtualization.framework.
// The registry is in-memory by design: vz virtual machines are child
// resources of this process, so a daemon crash takes its VMs with it —
// there is no orphaned-VM state to recover, and the durable lifecycle
// record lives in the append-only sandbox_events table (FR-17.18).
type Manager struct {
	cfg Config

	mu        sync.Mutex
	sandboxes map[string]*sandbox
	entropy   *ulid.MonotonicEntropy
}

// NewManager validates the configuration and returns a Manager. With a
// nil Hypervisor the platform implementation is selected; on non-darwin
// or CGO-free builds that is ErrSandboxBackendUnavailable.
func NewManager(cfg Config) (*Manager, error) {
	switch {
	case cfg.WorkDir == "":
		return nil, fmt.Errorf("vzf: work dir must not be empty")
	case cfg.Resolve == nil:
		return nil, fmt.Errorf("vzf: image resolver must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("vzf: logger must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("vzf: clock must not be nil")
	}
	if cfg.Hypervisor == nil {
		hv, err := newPlatformHypervisor()
		if err != nil {
			return nil, err
		}
		cfg.Hypervisor = hv
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("vzf: create work dir: %w", err)
	}
	return &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
		entropy:   ulid.Monotonic(rand.Reader, 0),
	}, nil
}

// Launch boots one microVM bound to the task's worktree and registers
// it before returning (the FR-17.26 ceiling depends on that ordering).
// Refs: FR-17.1, FR-17.3, FR-17.15, FR-17.17
func (m *Manager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("vzf launch: %w", err)
	}
	images, err := m.cfg.Resolve(opts.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("vzf launch: resolve image %q: %w", opts.ImageRef, err)
	}

	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("vzf launch: %w", err)
	}
	dir := filepath.Join(m.cfg.WorkDir, id)
	overlay, err := createOverlay(dir, opts.DiskQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("vzf launch: %w", err)
	}

	vm, err := m.cfg.Hypervisor.CreateVM(vmConfig{
		CPUs:           opts.CPUs,
		MemoryMB:       opts.MemoryMB,
		KernelPath:     images.KernelPath,
		RootfsPath:     images.RootfsPath,
		RootfsReadOnly: true, // the pinned image is immutable (FR-17.17)
		Cmdline:        images.Cmdline,
		OverlayPath:    overlay,
		WorktreePath:   opts.WorktreePath,
		WorktreeTag:    "work",
		AttachNIC:      opts.Network.Mode != model.NetworkModeNone,
		VsockEnabled:   true,
		BalloonEnabled: true,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("vzf launch: create vm: %w", err)
	}
	if err := vm.Start(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("vzf launch: start vm: %w", err)
	}

	now := m.cfg.Clock().UTC()
	info := model.SandboxInfo{
		ID:               id,
		TaskID:           opts.TaskID,
		WorktreePath:     opts.WorktreePath,
		Backend:          model.BackendVZF,
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

	m.mu.Lock()
	m.sandboxes[id] = &sandbox{info: info, vm: vm, dir: dir}
	m.mu.Unlock()

	m.cfg.Logger.Info("vzf sandbox launched", "event", "launched",
		"sandbox_id", id, "task_id", opts.TaskID, "network_mode", opts.Network.Mode)
	return &info, nil
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
// the mgit-guest agent's contract (MGIT-11.5.6); until it lands, exec
// reports honestly that the transport is missing rather than faking
// success. Refs: FR-17.11
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

// Stop halts the sandbox's VM and records it suspended: not running,
// resources held until Remove. v1 does not resume a stopped VM (the
// NFR-17.3 idle-suspend/resume cycle arrives with the lifecycle
// service, MGIT-11.9.5, using vz pause/resume).
func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if err := sb.vm.Stop(ctx, force); err != nil {
		return fmt.Errorf("vzf stop: %w", err)
	}
	m.mu.Lock()
	sb.info.State = model.StateSuspended
	m.mu.Unlock()
	return nil
}

// Remove tears the sandbox down: VM stopped, every sandbox-local file
// (overlay, state dir) deleted, registration dropped. The worktree is
// never touched (FR-17.19).
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State == model.StateRunning {
		if err := sb.vm.Stop(ctx, force); err != nil && !force {
			return fmt.Errorf("vzf remove: stop: %w", err)
		}
	}
	if err := os.RemoveAll(sb.dir); err != nil {
		return fmt.Errorf("vzf remove: clear sandbox dir: %w", err)
	}

	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()

	m.cfg.Logger.Info("vzf sandbox removed", "event", "removed", "sandbox_id", id)
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

// defaultOverlaySizeMB sizes the overlay when the request leaves the
// disk quota unresolved (NFR-17.5 default).
const defaultOverlaySizeMB = 4096

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
	file, err := os.OpenFile(overlay, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is built from manager-owned dir
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
