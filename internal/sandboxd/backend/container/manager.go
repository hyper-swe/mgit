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
//
// SEC-03: even as the reduced-isolation fallback, the container still
// QUARANTINES the store (MGIT-11.6.9): it never bind-mounts the live worktree
// (which could carry its own .mgit/.git). Each launch seeds a fresh private,
// sandbox-local .mgit store, builds the quarantine plan + binds the private
// store (fail closed on ErrSharedStoreReachable), stages the worktree (worktree
// files + the private .mgit, in-worktree stores excluded, escaping symlinks
// rejected) via the shared staging package, and bind-mounts the STAGED dir at
// the worktree's identical path. The host shared store is never reachable.
// Refs: FR-17.15, FR-17.3, FR-17.14, SEC-03, MGIT-11.6.9
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
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
	"github.com/hyper-swe/mgit/internal/sandboxd/quarantine"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
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
	// WorkDir is the sandbox-local state root (never a worktree) where per-
	// sandbox host artifacts live: the SEC-03 private store and the staged
	// worktree. Teardown clears each sandbox's subdir with one RemoveAll.
	// Required when StoreProvisioner is set. Refs: SEC-03, FR-17.19
	WorkDir string
	// StoreProvisioner seeds the SEC-03 private, sandbox-local store per launch
	// and supplies the shared store path for the non-reachability check. When
	// set, the quarantine control is REALIZED: each launch builds the plan,
	// binds the private store, fails closed (ErrSharedStoreReachable) on a leaky
	// layout, and mounts a STAGED worktree instead of the live one. When nil
	// (tests/direct path) the live worktree is mounted, the pre-SEC-03 behavior.
	// Refs: SEC-03, MGIT-11.6.9
	StoreProvisioner provision.Provisioner
	Logger           *slog.Logger
	Clock            func() time.Time
}

// containerSandbox is one supervised container.
type containerSandbox struct {
	info model.SandboxInfo
	name string // runtime container name
	dir  string // per-sandbox state dir (private store + staging); "" when unquarantined
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
	case cfg.StoreProvisioner != nil && cfg.WorkDir == "":
		// SEC-03 quarantine needs a sandbox-local state root to hold the private
		// store + staged worktree; without it the control cannot be realized.
		return nil, fmt.Errorf("container: work dir must not be empty when a store provisioner is set (SEC-03)")
	}
	if cfg.WorkDir != "" {
		if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
			return nil, fmt.Errorf("container: create work dir: %w", err)
		}
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

	// Honor a host-assigned lifecycle ID (lazy provisioning, FR-17.10);
	// generate one only for direct/legacy use.
	id := opts.SandboxID
	if id == "" {
		var gerr error
		if id, gerr = m.newID(); gerr != nil {
			return nil, fmt.Errorf("container launch: %w", gerr)
		}
	}
	name := "mgit-sbx-" + id

	// SEC-03: provision the private store, prove the shared store unreachable,
	// and stage the worktree BEFORE the container exists. A quarantine failure
	// fails the launch closed (e.g. ErrSharedStoreReachable / ErrSymlinkEscape);
	// no container ever runs against a leaky or unstaged worktree. stateDir is
	// "" and mountDir is the live worktree only when no provisioner is wired
	// (tests/direct path). Refs: SEC-03, FR-17.3
	stateDir, mountDir, err := m.quarantine(id, opts)
	if err != nil {
		return nil, fmt.Errorf("container launch: %w", err)
	}

	if _, err := m.cfg.Runner.run(ctx, m.runArgs(name, network, mountDir, opts)...); err != nil {
		if stateDir != "" {
			_ = os.RemoveAll(stateDir)
		}
		return nil, fmt.Errorf("container launch: %w", err)
	}

	info := m.newSandboxInfo(id, opts)

	m.mu.Lock()
	m.sandboxes[id] = &containerSandbox{info: info, name: name, dir: stateDir}
	m.mu.Unlock()

	m.cfg.Logger.Info("container sandbox launched (REDUCED ISOLATION)",
		"event", "launched", "sandbox_id", id, "task_id", opts.TaskID,
		"reduced_isolation", true)
	return &info, nil
}

// stateDirName / privateStoreDirName / stagingDirName name the per-sandbox host
// artifacts under WorkDir/<id> (cleaned by Remove's one RemoveAll).
const (
	privateStoreDirName = "private-store"
	stagingDirName      = "worktree-staging"
)

// quarantine realizes the SEC-03 control for one container launch when a
// provisioner is wired: it seeds a fresh private .mgit store under a per-
// sandbox state dir, builds the guest filesystem plan + binds the private store
// (rejecting the launch with ErrSharedStoreReachable on a reachable shared
// store), then STAGES the worktree (worktree files + the private .mgit, in-
// worktree stores excluded, escaping symlinks rejected). It returns the state
// dir (for teardown) and the directory to bind-mount at the worktree path: the
// staged dir when quarantined, else the live worktree (no provisioner — tests/
// direct path). On any failure it removes the partial state dir (fail closed).
// Refs: SEC-03, FR-17.3, FR-17.5, FR-17.14
func (m *Manager) quarantine(id string, opts model.SandboxLaunchOptions) (stateDir, mountDir string, err error) {
	if m.cfg.StoreProvisioner == nil {
		return "", opts.WorktreePath, nil // quarantine not wired (tests/direct path)
	}
	stateDir = filepath.Join(m.cfg.WorkDir, id)
	if mkErr := os.MkdirAll(stateDir, 0o700); mkErr != nil {
		return "", "", fmt.Errorf("create sandbox state dir: %w", mkErr)
	}
	store, pErr := m.cfg.StoreProvisioner.Provision(opts.TaskID, filepath.Join(stateDir, privateStoreDirName))
	if pErr != nil {
		_ = os.RemoveAll(stateDir)
		return "", "", fmt.Errorf("provision private store: %w", pErr)
	}
	plan, bpErr := quarantine.BuildPlan(opts.WorktreePath, m.cfg.SensitivePaths)
	if bpErr != nil {
		_ = os.RemoveAll(stateDir)
		return "", "", fmt.Errorf("build quarantine plan: %w", bpErr)
	}
	// BindPrivateStore returns ErrSharedStoreReachable for a leaky layout; the
	// caller rejects the launch, so a reachable shared store never runs.
	if _, bErr := plan.BindPrivateStore(store.Dir, store.SharedDir); bErr != nil {
		_ = os.RemoveAll(stateDir)
		return "", "", fmt.Errorf("bind private store: %w", bErr)
	}
	stageDir := filepath.Join(stateDir, stagingDirName)
	if sErr := staging.Build(opts.WorktreePath, store.Dir, stageDir); sErr != nil {
		_ = os.RemoveAll(stateDir)
		return "", "", fmt.Errorf("stage worktree: %w", sErr)
	}
	return stateDir, stageDir, nil
}

// runArgs builds the `run` argv: srcDir (the SEC-03 STAGED worktree when
// quarantined, else the live worktree) is the ONLY writable mount, at the
// worktree's identical GUEST path (FR-17.3); host-trusted paths are mounted
// read-only (FR-17.14); resource caps applied (FR-17.26). The guest always sees
// the worktree at opts.WorktreePath; only the host SOURCE differs (staged vs.
// live), so a quarantined container can never reach the live worktree's own
// store. Refs: FR-17.3, FR-17.14, SEC-03
func (m *Manager) runArgs(name, network, srcDir string, opts model.SandboxLaunchOptions) []string {
	args := make([]string, 0, 16+2*len(m.cfg.SensitivePaths))
	args = append(args,
		"run", "--detach", "--name", name,
		"--network", network,
		"--memory", strconv.Itoa(effectiveMemoryMB(opts.MemoryMB))+"m",
		"--cpus", strconv.Itoa(effectiveCPUs(opts.CPUs)),
		"--volume", srcDir+":"+opts.WorktreePath,
	)
	args = append(args, sensitiveMounts(srcDir, opts.WorktreePath, m.cfg.SensitivePaths)...)
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

	// Clear the per-sandbox state dir (the SEC-03 private store + staged
	// worktree). It is sandbox-local host state under WorkDir, never the live
	// worktree, so teardown leaves no residue and never touches the worktree
	// (FR-17.19). Empty for unquarantined launches (tests/direct path).
	if sb.dir != "" {
		if err := os.RemoveAll(sb.dir); err != nil {
			return fmt.Errorf("container remove: clear sandbox dir: %w", err)
		}
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

// sensitiveMounts remounts existing host-trusted paths read-only over the
// writable worktree mount (FR-17.14). The host SOURCE is taken from srcDir (the
// staged worktree when quarantined, else the live worktree); the read-only
// GUEST target is at the worktree's identical path (guestWorktree), so the
// guest sees them where it expects but cannot modify them. Missing paths are
// skipped: they cannot be modified if they do not exist. Refs: FR-17.14, SEC-03
func sensitiveMounts(srcDir, guestWorktree string, sensitive []string) []string {
	var args []string
	for _, rel := range sensitive {
		src := filepath.Join(srcDir, rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(guestWorktree, rel)
		args = append(args, "--volume", src+":"+dst+":ro")
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
