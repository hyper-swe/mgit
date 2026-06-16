// Package container is the REDUCED-ISOLATION sandbox fallback
// (FR-17.15): a rootless OS container instead of a hardware-isolated
// microVM — the kernel is shared with the host, so one kernel bug is
// an escape. It exists only behind the explicit, audited
// --backend container --acknowledge-reduced-isolation selection
// (sandboxd.SelectBackend); nothing ever picks it automatically.
// The contract it CAN keep is kept: worktree-only mount at the
// identical path, read-only sensitive paths (FR-17.14), resource caps,
// and honest network mapping — allowlist mode is refused until the
// host egress proxy exists rather than silently widened (SEC-04).
// Refs: FR-17.15, FR-17.3, FR-17.14
package container

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/model"
)

// Runner executes one container-runtime CLI invocation (rootless
// podman). Faked in tests; PodmanRunner is the real implementation.
type Runner interface {
	run(ctx context.Context, args ...string) ([]byte, error)
}

// Config wires the manager's dependencies.
type Config struct {
	Runner         Runner   // container runtime CLI
	SensitivePaths []string // host-trusted paths remounted read-only (FR-17.14)
	Logger         *slog.Logger
	Clock          func() time.Time
}

// containerSandbox is one supervised container.
type containerSandbox struct {
	info model.SandboxInfo
	name string // runtime container name
}

// Manager implements model.SandboxManager on a rootless container
// runtime. The registry is in-memory: containers run with --rm
// semantics owned by this daemon process.
type Manager struct {
	cfg Config

	mu        sync.Mutex
	sandboxes map[string]*containerSandbox
	entropy   *ulid.MonotonicEntropy
}

// NewManager validates the configuration and returns a Manager.
func NewManager(cfg Config) (*Manager, error) {
	switch {
	case cfg.Runner == nil:
		return nil, fmt.Errorf("container: runner must not be nil")
	case cfg.Logger == nil:
		return nil, fmt.Errorf("container: logger must not be nil")
	case cfg.Clock == nil:
		return nil, fmt.Errorf("container: clock must not be nil")
	}
	return &Manager{
		cfg:       cfg,
		sandboxes: make(map[string]*containerSandbox),
		entropy:   ulid.Monotonic(rand.Reader, 0),
	}, nil
}

// Launch starts one rootless container bound to the task's worktree
// and registers it before returning. Refs: FR-17.3, FR-17.7, FR-17.14
func (m *Manager) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("container launch: %w", err)
	}
	network, err := networkArg(opts.Network.Mode)
	if err != nil {
		return nil, err
	}

	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("container launch: %w", err)
	}
	name := "mgit-sbx-" + id

	if _, err := m.cfg.Runner.run(ctx, m.runArgs(name, network, opts)...); err != nil {
		return nil, fmt.Errorf("container launch: %w", err)
	}

	info := m.newSandboxInfo(id, opts)

	m.mu.Lock()
	m.sandboxes[id] = &containerSandbox{info: info, name: name}
	m.mu.Unlock()

	m.cfg.Logger.Info("container sandbox launched (REDUCED ISOLATION)",
		"event", "launched", "sandbox_id", id, "task_id", opts.TaskID,
		"reduced_isolation", true)
	return &info, nil
}

// runArgs builds the `run` argv: the worktree is the ONLY writable
// mount, at the identical path (FR-17.3); host-trusted paths are mounted
// read-only (FR-17.14); resource caps applied (FR-17.26).
func (m *Manager) runArgs(name, network string, opts model.SandboxLaunchOptions) []string {
	args := make([]string, 0, 16+2*len(m.cfg.SensitivePaths))
	args = append(args,
		"run", "--detach", "--name", name,
		"--network", network,
		"--memory", strconv.Itoa(effectiveMemoryMB(opts.MemoryMB))+"m",
		"--cpus", strconv.Itoa(effectiveCPUs(opts.CPUs)),
		"--volume", opts.WorktreePath+":"+opts.WorktreePath,
	)
	args = append(args, sensitiveMounts(opts.WorktreePath, m.cfg.SensitivePaths)...)
	return append(args, "--workdir", opts.WorktreePath, opts.ImageRef, "sleep", "infinity")
}

// newSandboxInfo assembles the running container's metadata record.
func (m *Manager) newSandboxInfo(id string, opts model.SandboxLaunchOptions) model.SandboxInfo {
	now := m.cfg.Clock().UTC()
	info := model.SandboxInfo{
		ID:               id,
		TaskID:           opts.TaskID,
		WorktreePath:     opts.WorktreePath,
		Backend:          model.BackendContainer,
		ImageDigest:      imageDigest(opts.ImageRef),
		NetworkMode:      opts.Network.Mode,
		NetworkAllowlist: opts.Network.Allowlist,
		State:            model.StateRunning,
		MemoryMB:         effectiveMemoryMB(opts.MemoryMB),
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

// Exec runs one whole command inside the container.
// Refs: FR-17.11
func (m *Manager) Exec(ctx context.Context, id string, req model.ExecRequest) (*model.ExecResult, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("container exec: %w", err)
	}
	sb, err := m.lookup(id)
	if err != nil {
		return nil, err
	}

	// Explicit per-exec env injections only (FR-17.17); the host
	// environment is never inherited — podman exec does not forward it,
	// and dropping req.Env would silently lose a task's injected creds.
	args := []string{"exec"}
	for _, env := range req.Env {
		args = append(args, "--env", env)
	}
	args = append(args, sb.name)
	args = append(args, req.Command...)
	out, err := m.cfg.Runner.run(ctx, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &model.ExecResult{Stdout: out, Stderr: exitErr.Stderr, ExitCode: exitErr.ExitCode()}, nil
		}
		return nil, fmt.Errorf("container exec: %w", err)
	}
	return &model.ExecResult{Stdout: out, ExitCode: 0}, nil
}

// Stop halts the container (state suspended; resources held until
// Remove).
func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	args := []string{"stop"}
	if force {
		args = append(args, "--time", "0")
	}
	args = append(args, sb.name)
	if _, err := m.cfg.Runner.run(ctx, args...); err != nil {
		return fmt.Errorf("container stop: %w", err)
	}
	m.mu.Lock()
	sb.info.State = model.StateSuspended
	m.mu.Unlock()
	return nil
}

// Remove destroys the container and deregisters it; the worktree is
// never touched (FR-17.19).
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	sb, err := m.lookup(id)
	if err != nil {
		return err
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, sb.name)
	if _, err := m.cfg.Runner.run(ctx, args...); err != nil {
		return fmt.Errorf("container remove: %w", err)
	}

	m.mu.Lock()
	delete(m.sandboxes, id)
	m.mu.Unlock()
	m.cfg.Logger.Info("container sandbox removed", "event", "removed", "sandbox_id", id)
	return nil
}

// Resolve returns one sandbox by id.
func (m *Manager) Resolve(_ context.Context, id string) (*model.SandboxInfo, error) {
	sb, err := m.lookup(id)
	if err != nil {
		return nil, err
	}
	info := sb.info
	return &info, nil
}

// lookup fetches a registered sandbox.
func (m *Manager) lookup(id string) (*containerSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb, ok := m.sandboxes[id]; ok {
		return sb, nil
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

// networkArg maps the FR-17.7 mode onto the rootless runtime. The
// allowlist mode is refused, not approximated: the runtime cannot do
// IP/flow-layer default-deny, and silently mapping allowlist to an
// open user network would be exactly the SEC-04 false-containment the
// audit rejected. It becomes available with the host egress proxy
// (MGIT-11.7.2).
func networkArg(mode string) (string, error) {
	switch mode {
	case model.NetworkModeNone:
		return "none", nil
	case model.NetworkModeOpen:
		return "slirp4netns", nil
	default:
		return "", fmt.Errorf("%w: allowlist mode on the container backend requires the host egress proxy (MGIT-11.7.2)",
			model.ErrNetworkPolicyViolation)
	}
}

// sensitiveMounts remounts existing host-trusted paths read-only over
// the writable worktree mount (FR-17.14). Missing paths are skipped:
// they cannot be modified if they do not exist.
func sensitiveMounts(worktree string, sensitive []string) []string {
	var args []string
	for _, rel := range sensitive {
		abs := filepath.Join(worktree, rel)
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		args = append(args, "--volume", abs+":"+abs+":ro")
	}
	return args
}

// effectiveMemoryMB resolves the NFR-17.5 default for unset caps.
func effectiveMemoryMB(requested int) int {
	if requested <= 0 {
		return 2048
	}
	return requested
}

// effectiveCPUs resolves the NFR-17.5 default for unset caps.
func effectiveCPUs(requested int) int {
	if requested <= 0 {
		return 2
	}
	return requested
}

// imageDigest extracts the digest from a pinned reference.
func imageDigest(imageRef string) string {
	for i, r := range imageRef {
		if r == '@' {
			return imageRef[i+1:]
		}
	}
	return ""
}

// PodmanRunner executes rootless podman. The container runtime binary
// is COTS, assessed per FR-17.30/FR-17.31.
type PodmanRunner struct{}

// run invokes podman with the given arguments.
func (PodmanRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "podman", args...).Output() //nolint:gosec // argv built from validated options, no shell
	if err != nil {
		return out, fmt.Errorf("podman %s: %w", args[0], err)
	}
	return out, nil
}
