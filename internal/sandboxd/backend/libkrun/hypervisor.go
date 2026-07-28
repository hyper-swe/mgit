package libkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

const (
	// guestInitPath is the guest PID-1 workload, guest-root-relative: the
	// mgit-guest supervisor, same as every other microVM backend.
	guestInitPath = "/sbin/mgit-guest"

	// consoleLogName is the per-VM guest console capture under the state dir
	// (same name as the firecracker backend's).
	consoleLogName = "console.log"

	// startTimeout bounds the wait for the child's "configured, entering"
	// handshake. Configuration is host-side work (bind sockets, build a
	// netstack, configure libkrun) measured in milliseconds; a child silent
	// for this long is stuck, not slow.
	startTimeout = 30 * time.Second

	// stopGrace is how long a graceful Stop waits after SIGTERM before
	// escalating to SIGKILL.
	stopGrace = 5 * time.Second
)

// childProcess is one spawned re-exec VM child, seamed so the lifecycle is
// testable without real processes (and without libkrun).
type childProcess interface {
	// Handshake is the read end of the child's fd-3 progress pipe. EOF means
	// the child exited.
	Handshake() io.Reader
	// Signal delivers sig to the child.
	Signal(sig os.Signal) error
	// Kill terminates the child immediately.
	Kill() error
	// Wait reaps the child and returns its exit code. It must be called
	// exactly once and blocks until exit.
	Wait() (int, error)
}

// spawnFunc starts one VM child for a spec, its console wired to consolePath.
type spawnFunc func(exePath string, spec vmSpec, consolePath string) (childProcess, error)

// Hypervisor implements microvm.Hypervisor by re-exec: every VM is a child
// process running the daemon's own binary under ChildCommand, because
// krun_start_enter seizes and exit()s its process (ADR-010). This is
// firecracker's proven one-VMM-process-per-sandbox shape — with the daemon
// binary itself as the VMM process, so there is no second artifact to build,
// sign or ship, and the macOS hypervisor entitlement (attached to the signed
// binary) carries over by construction. Refs: ADR-010, FR-17.15, MGIT-61.8
type Hypervisor struct {
	exePath string
	spawn   spawnFunc
	logger  *slog.Logger
}

// NewHypervisor returns the libkrun Hypervisor. It resolves the daemon's own
// executable once: if the binary cannot re-exec itself, no VM can ever start,
// so fail at construction rather than at the first launch.
func NewHypervisor(logger *slog.Logger) (*Hypervisor, error) {
	if logger == nil {
		return nil, fmt.Errorf("libkrun hypervisor: logger must not be nil")
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: libkrun re-exec: resolve own executable: %w",
			model.ErrSandboxBackendUnavailable, err)
	}
	return &Hypervisor{exePath: exePath, spawn: spawnChild, logger: logger}, nil
}

// CreateVM validates the config and returns the VM handle. Everything that
// can be rejected host-side is rejected HERE, before any process spawns:
// a refusal with a reason always beats a child that dies opaquely.
// Refs: FR-17.7, SEC-03, SEC-04, ADR-010
func (h *Hypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	// SEC-03 fail-closed: the quarantined (staged worktree + private store)
	// delivery is not implemented on libkrun yet. Launching anyway would
	// share the LIVE worktree with no store quarantine — silently weaker than
	// every other backend. Better no sandbox than an unquarantined one.
	if cfg.PrivateStorePath != "" {
		return nil, fmt.Errorf(
			"%w: the libkrun backend does not deliver the SEC-03 private-store "+
				"quarantine yet (MGIT-61.6); refusing to launch without it",
			model.ErrSandboxBackendUnavailable)
	}
	spec := vmSpec{
		SandboxID:    cfg.SandboxID,
		TaskID:       cfg.TaskID,
		CPUs:         cfg.CPUs,
		MemoryMB:     cfg.MemoryMB,
		StateDir:     cfg.StateDir,
		RootDir:      cfg.RootfsPath,
		RootReadOnly: cfg.RootfsReadOnly,
		WorktreePath: cfg.WorktreePath,
		WorktreeTag:  cfg.WorktreeTag,
		VsockEnabled: cfg.VsockEnabled,
		ExecPath:     guestInitPath,
		ExecArgs:     guestInitArgs(cfg.VsockEnabled),
		ExecEnv:      guestEnv(),
		NetworkMode:  cfg.NetworkMode,
		Allowlist:    cfg.NetworkAllowlist,
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &krunVM{
		spec:        spec,
		exePath:     h.exePath,
		spawn:       h.spawn,
		logger:      h.logger,
		consolePath: filepath.Join(cfg.StateDir, consoleLogName),
	}, nil
}

// guestInitArgs are the arguments to the guest supervisor — the executable
// itself is NOT included, because libkrun prepends it to argv (measured;
// ADR-010). The vsock ports are passed EXPLICITLY rather than left to
// mgit-guest's flag defaults, so the host and guest cannot silently disagree
// if a default ever moves: both sides read microvm's constants. A VM without
// a vsock control plane disables the notify trigger (port 0). Refs: FR-17.11
func guestInitArgs(vsockEnabled bool) []string {
	if !vsockEnabled {
		return []string{"--notify-host-port", "0"}
	}
	return []string{
		"--vsock-port", strconv.FormatUint(uint64(microvm.GuestExecPort), 10),
		"--land-vsock-port", strconv.FormatUint(uint64(microvm.GuestLandPort), 10),
		"--notify-host-port", strconv.FormatUint(uint64(microvm.GuestNotifyPort), 10),
	}
}

// guestEnv is the guest workload's environment. It is set explicitly — and
// deliberately minimal — because libkrun's convenience of accepting a NULL
// envp collects the CALLING process's environment, which would carry daemon
// state into the guest (SEC-05). Refs: SEC-05
func guestEnv() []string {
	return []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin"}
}

// krunVM adapts one re-exec child process to the manager's lifecycle seam.
type krunVM struct {
	spec        vmSpec
	exePath     string
	spawn       spawnFunc
	logger      *slog.Logger
	consolePath string

	mu       sync.Mutex
	child    childProcess
	waitDone chan struct{} // closed once the child is reaped (watch goroutine)
}

// Start spawns the VM child and waits for its "configured, entering"
// handshake. On any failure it reaps the child itself — Manager.Launch never
// calls Stop after a failed Start, so the VM must self-clean (no orphan
// process; the child's own failure path released its sockets, and the state
// dir is the manager's to remove). Refs: SEC-10, MGIT-61.8
func (v *krunVM) Start(ctx context.Context) error {
	child, err := v.spawn(v.exePath, v.spec, v.consolePath)
	if err != nil {
		return fmt.Errorf("libkrun spawn vm child: %w", err)
	}
	dec := json.NewDecoder(child.Handshake())

	hs, err := readHandshake(ctx, dec)
	if err != nil || !hs.OK {
		_ = child.Kill()
		code, _ := child.Wait()
		reason := err
		if err == nil {
			reason = errors.New(hs.Error)
		}
		return fmt.Errorf("libkrun vm failed to configure (child exit %d, console: %s): %w",
			code, v.consolePath, reason)
	}

	done := make(chan struct{})
	v.mu.Lock()
	v.child, v.waitDone = child, done
	v.mu.Unlock()
	go v.watch(child, dec, done)
	return nil
}

// readHandshake reads one handshake line, bounded by the caller's ctx and by
// startTimeout — whichever comes first. The decode goroutine is not orphaned:
// every abandonment path in Start kills and reaps the child, which closes the
// pipe and unblocks it.
func readHandshake(ctx context.Context, dec *json.Decoder) (childHandshake, error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	type result struct {
		hs  childHandshake
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var hs childHandshake
		err := dec.Decode(&hs)
		if errors.Is(err, io.EOF) {
			err = errors.New("vm child exited before reporting (see its console log)")
		}
		ch <- result{hs, err}
	}()
	select {
	case r := <-ch:
		return r.hs, r.err
	case <-ctx.Done():
		return childHandshake{}, fmt.Errorf("waiting for vm child (bounded by %s): %w", startTimeout, ctx.Err())
	}
}

// watch drains any post-boot handshake line (a pre-guest boot failure),
// reaps the child, and records the outcome. The child's exit code IS the
// guest workload's exit code — except when a late handshake error arrived
// (boot never reached the guest) or the code is one libkrun's init reserves.
// Refs: ADR-010, MGIT-61.8
func (v *krunVM) watch(child childProcess, dec *json.Decoder, done chan struct{}) {
	defer close(done)
	var late childHandshake
	lateErr := dec.Decode(&late) // EOF when the VM ran to exit without incident
	code, waitErr := child.Wait()

	switch {
	case lateErr == nil && !late.OK:
		v.logger.Error("libkrun vm boot failed before the guest ran",
			"event", "krun_vm_bootfail", "sandbox_id", v.spec.SandboxID,
			"exit_code", code, "error", late.Error, "console", v.consolePath)
	case isGuestInitFailure(code):
		v.logger.Error("libkrun vm guest init failed",
			"event", "krun_vm_initfail", "sandbox_id", v.spec.SandboxID,
			"exit_code", code, "console", v.consolePath)
	default:
		v.logger.Info("libkrun vm exited",
			"event", "krun_vm_exit", "sandbox_id", v.spec.SandboxID,
			"exit_code", code, "wait_error", waitErrString(waitErr))
	}
}

// isGuestInitFailure reports whether a post-handshake exit code is one
// libkrun's in-guest init reserves (125 init failure, 126 workload not
// executable, 127 workload not found) rather than a workload exit.
func isGuestInitFailure(code int) bool {
	return code == 125 || code == 126 || code == 127
}

// waitErrString renders a Wait error for logging; a normal non-zero exit is
// not an error here (the code carries it).
func waitErrString(err error) string {
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) {
		return ""
	}
	return err.Error()
}

// Stop halts the VM by stopping its child process: SIGTERM first (libkrun
// tears the VM down with the process), escalating to SIGKILL after stopGrace
// or when force is set. There is no guest-cooperative shutdown across the
// process boundary today — teardown IS process exit, which is also what
// guarantees the child's sockets and netstack die with it (SEC-10).
func (v *krunVM) Stop(ctx context.Context, force bool) error {
	v.mu.Lock()
	child, done := v.child, v.waitDone
	v.mu.Unlock()
	if child == nil {
		return nil // never started (or Start failed and already reaped)
	}
	select {
	case <-done:
		return nil // already exited and reaped
	default:
	}

	if force {
		_ = child.Kill()
	} else if err := child.Signal(syscall.SIGTERM); err != nil {
		_ = child.Kill()
	}

	grace := stopGrace
	if deadline, ok := ctx.Deadline(); ok {
		if until := time.Until(deadline); until < grace {
			grace = until
		}
	}
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = child.Kill()
	<-done // Kill guarantees exit; watch reaps it
	return nil
}

// PeerIdentity reports the host-observed per-VM identity: the exec vsock
// unix socket path. Like firecracker's, the path is host-created, private,
// and unique per sandbox (under its state dir), so it genuinely
// distinguishes guest A from guest B; it is never guest-asserted (SEC-05).
// Empty when the VM has no vsock control plane. Refs: SEC-10, SEC-05
func (v *krunVM) PeerIdentity() string {
	if !v.spec.VsockEnabled {
		return ""
	}
	return vsockSocketPath(v.spec.StateDir, microvm.GuestExecPort)
}

// NotifySocketPath reports the per-VM host socket for the guest->host
// land-ready notification: libkrun connects to this path when the guest
// dials the notify vsock port, and the daemon listens on it (the same
// direction as firecracker's "<vsock>_<port>" convention). Empty without a
// vsock control plane. Refs: MGIT-11.10.11, SEC-10
func (v *krunVM) NotifySocketPath() string {
	if !v.spec.VsockEnabled {
		return ""
	}
	return vsockSocketPath(v.spec.StateDir, microvm.GuestNotifyPort)
}

// spawnChild is the real spawnFunc: it re-execs the daemon binary under
// ChildCommand with the spec on stdin, console output captured to a per-VM
// file, and the handshake pipe on fd 3. The daemon's own stdio is NEVER
// inherited — krun_start_enter hands the child's stdin/stdout to the guest,
// and a hostile guest must not hold the daemon's streams. Refs: SEC-10, ADR-010
func spawnChild(exePath string, spec vmSpec, consolePath string) (childProcess, error) {
	cmd, handshakeR, cleanup, err := newChildCmd(exePath, spec, consolePath)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		_ = handshakeR.Close()
		return nil, fmt.Errorf("start vm child: %w", err)
	}
	// The parent's copies of the child-held files are closed once the child
	// owns them; the read end of the handshake pipe stays.
	cleanup()
	return &execChild{cmd: cmd, handshake: handshakeR}, nil
}

// newChildCmd builds the child command with its full stdio contract. Split
// from spawnChild so tests can assert the wiring — spec on stdin, console
// file (never the daemon's streams) on stdout/stderr, handshake pipe as the
// sole extra fd — without spawning anything. The returned cleanup closes the
// parent's copies of the files handed to the child.
func newChildCmd(exePath string, spec vmSpec, consolePath string) (*exec.Cmd, *os.File, func(), error) {
	var specJSON bytes.Buffer
	if err := spec.encode(&specJSON); err != nil {
		return nil, nil, nil, err
	}
	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // path built from the manager-owned state dir
	if err != nil {
		return nil, nil, nil, fmt.Errorf("vm console log: %w", err)
	}
	handshakeR, handshakeW, err := os.Pipe()
	if err != nil {
		_ = console.Close()
		return nil, nil, nil, fmt.Errorf("vm handshake pipe: %w", err)
	}

	// No context: the child's lifetime is the VM's (minutes to hours), owned
	// by krunVM.Stop via signals — a ctx cancellation killing the VMM behind
	// the lifecycle's back is exactly what must not happen.
	cmd := exec.Command(exePath, ChildCommand) //nolint:gosec,noctx // exePath is the daemon's own resolved binary; lifetime owned by Stop, not a ctx
	// A finite, parent-closed stream: the guest inherits an EOF'd pipe as its
	// stdin, never a live daemon descriptor.
	cmd.Stdin = &specJSON
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.ExtraFiles = []*os.File{handshakeW} // fd 3 in the child
	// The child gets a MINIMAL environment: the daemon's env must not leak
	// toward the guest, and the child needs nothing from it. (The guest's own
	// env comes from the spec, independent of this.) On macOS the dynamic
	// loader still needs any DYLD fallback the daemon itself was started
	// with, so that one variable is forwarded when present (libkrunfw is
	// dlopened by leaf name — ADR-010 packaging finding).
	cmd.Env = childEnv(os.Getenv("DYLD_FALLBACK_LIBRARY_PATH"))

	cleanup := func() {
		_ = console.Close()
		_ = handshakeW.Close()
	}
	return cmd, handshakeR, cleanup, nil
}

// childEnv builds the child's minimal environment.
func childEnv(dyldFallback string) []string {
	env := []string{"PATH=/usr/bin:/bin"}
	if dyldFallback != "" {
		env = append(env, "DYLD_FALLBACK_LIBRARY_PATH="+dyldFallback)
	}
	return env
}

// execChild adapts a real *exec.Cmd to childProcess.
type execChild struct {
	cmd       *exec.Cmd
	handshake *os.File
}

func (c *execChild) Handshake() io.Reader { return c.handshake }

func (c *execChild) Signal(sig os.Signal) error { return c.cmd.Process.Signal(sig) }

func (c *execChild) Kill() error { return c.cmd.Process.Kill() }

// Wait reaps the child, closes the parent's handshake end, and returns the
// exit code (-1 when killed by signal, per os.ProcessState).
func (c *execChild) Wait() (int, error) {
	err := c.cmd.Wait()
	_ = c.handshake.Close()
	if c.cmd.ProcessState == nil {
		return -1, err
	}
	return c.cmd.ProcessState.ExitCode(), err
}
