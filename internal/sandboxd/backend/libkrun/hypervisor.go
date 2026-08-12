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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
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
	return newHypervisor(logger, newCapabilityProbe())
}

// newHypervisor is NewHypervisor with the capability probe injected, so the
// fail-closed path can be tested without a deliberately-broken libkrun.
func newHypervisor(logger *slog.Logger, probe netCapabilityProbe) (*Hypervisor, error) {
	if logger == nil {
		return nil, fmt.Errorf("libkrun hypervisor: logger must not be nil")
	}
	// Verify the linked libkrun can attach a NIC BEFORE anything else: every
	// sandbox needs one in every mode, so a libkrun without networking can
	// serve no launch at all, and refusing here reports it once at startup
	// rather than once per doomed launch. Refs: MGIT-61.14, ADR-010
	if err := requireNetworking(probe); err != nil {
		return nil, err
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: libkrun re-exec: resolve own executable: %w",
			model.ErrSandboxBackendUnavailable, err)
	}
	// Name the VMM and the capabilities actually verified, so an operator can
	// tell from the log that networking was checked — a package that silently
	// drops NET=1 is otherwise invisible until a guest leaks (MGIT-61.14).
	logger.Info("sandbox VMM capabilities verified",
		"event", "vmm_capabilities", "vmm", model.BackendLibkrun,
		"networking", "available", "vm_model", "re-exec child per VM")
	return &Hypervisor{exePath: exePath, spawn: spawnChild, logger: logger}, nil
}

// CreateVM validates the config and returns the VM handle. Everything that
// can be rejected host-side is rejected HERE, before any process spawns:
// a refusal with a reason always beats a child that dies opaquely.
// Refs: FR-17.7, SEC-03, SEC-04, ADR-010
func (h *Hypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	// SEC-03: deliver the QUARANTINED tree, not the live worktree. This runs
	// host-side and BEFORE the VM exists, so an escaping symlink or a leaky
	// layout fails the launch rather than reaching a booted guest.
	hostDir, err := deliverWorktree(cfg)
	if err != nil {
		return nil, err
	}
	spec := vmSpec{
		SandboxID:       cfg.SandboxID,
		TaskID:          cfg.TaskID,
		CPUs:            cfg.CPUs,
		MemoryMB:        cfg.MemoryMB,
		StateDir:        cfg.StateDir,
		RootDir:         cfg.RootfsPath,
		RootReadOnly:    cfg.RootfsReadOnly,
		WorktreeHostDir: hostDir,
		WorktreePath:    cfg.WorktreePath,
		WorktreeTag:     cfg.WorktreeTag,
		VsockEnabled:    cfg.VsockEnabled,
		PublishPorts:    cfg.PublishPorts,
		ExecPath:        guestInitPath,
		ExecArgs:        guestInitArgs(cfg.VsockEnabled),
		ExecEnv:         guestEnv(cfg),
		NetworkMode:     cfg.NetworkMode,
		Allowlist:       cfg.NetworkAllowlist,
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

// stagingDirName is the per-VM SEC-03 staging tree, under the sandbox state
// dir so teardown's single RemoveAll reclaims it (FR-17.19). Same name and
// role as vzf's. Refs: SEC-03
const stagingDirName = "worktree-staging"

// deliverWorktree resolves the HOST directory shared into the guest.
//
// With a private store wired (SEC-03) it builds the quarantined staging tree
// — worktree files plus the private .mgit, with any in-worktree git/mgit
// store excluded and escaping symlinks REJECTED — and shares that. Without
// one (the documented pre-SEC-03 direct path, and tests) the worktree itself
// is shared unchanged.
//
// Why a staged copy rather than a live share, restating the ADR-005 reasoning
// so the next reader does not "optimize" it away: virtiofs has no per-entry
// deny and no symlink-resolution boundary, so a live share cannot exclude an
// in-worktree store, rebind .mgit to the sandbox-local one, or reject an
// escaping symlink before the guest follows it. A staged copy enforces every
// invariant host-side, before the guest boots. Refs: SEC-03, FR-17.3, ADR-005
func deliverWorktree(cfg microvm.VMConfig) (string, error) {
	if cfg.WorktreePath == "" || cfg.PrivateStorePath == "" {
		return cfg.WorktreePath, nil
	}
	if cfg.StateDir == "" {
		return "", fmt.Errorf(
			"%w: libkrun needs a sandbox state dir to stage the quarantined worktree into",
			model.ErrSandboxBackendUnavailable)
	}
	stagingDir := filepath.Join(cfg.StateDir, stagingDirName)
	if err := staging.Build(cfg.WorktreePath, cfg.PrivateStorePath, stagingDir); err != nil {
		return "", fmt.Errorf("libkrun worktree quarantine: %w", err)
	}
	return stagingDir, nil
}

// guestEnv is the guest workload's environment. It is set explicitly — and
// deliberately minimal — because libkrun's convenience of accepting a NULL
// envp collects the CALLING process's environment, which would carry daemon
// state into the guest (SEC-05).
//
// It also carries the host->guest BOOT TOKENS. libkrun boots libkrunfw's own
// kernel, so unlike firecracker and vzf there is no command line to append
// the FR-17.3 worktree descriptor to; the guest reads the identical tokens
// from this variable instead (guestboot.BootTokens). Without it the share is
// attached but never mounted, and the guest sees an empty worktree path.
//
// The same channel carries the guest's NETWORK descriptor (MGIT-68). It has
// to: firecracker and vzf can hand the guest an address on the kernel command
// line, and libkrun has no command line of ours at all — which is precisely
// why this backend shipped with an unconfigured guest NIC.
// Refs: SEC-05, FR-17.3, MGIT-68, ADR-010
func guestEnv(cfg microvm.VMConfig) []string {
	env := []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin"}
	tokens := guestboot.AppendCmdline("", guestboot.WorktreeMount{
		Path: cfg.WorktreePath, FSType: "virtiofs", Source: cfg.WorktreeTag,
	})
	tokens = guestboot.AppendPublishPortsCmdline(tokens, cfg.PublishPorts)
	tokens = guestboot.AppendNetworkCmdline(tokens, guestNetworkFor(cfg.NetworkMode))
	if tokens != "" {
		env = append(env, guestboot.EnvBootTokens+"="+tokens)
	}
	return env
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

// ConsoleTail returns the tail of this VM's captured guest console, which is
// what a launch that fails closed quotes as its diagnosis. Refs: MGIT-92
func (v *krunVM) ConsoleTail(maxBytes int) string {
	return microvm.TailFile(v.consolePath, maxBytes)
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
	// env comes from the spec, independent of this.)
	cmd.Env = childEnv(os.Getenv, libkrunfwDirs)

	cleanup := func() {
		_ = console.Close()
		_ = handshakeW.Close()
	}
	return cmd, handshakeR, cleanup, nil
}

// childEnv builds the child's minimal environment.
// libLoaderPathVars are the dynamic-loader search paths forwarded from the
// daemon to the VM child.
//
// libkrun dlopens libkrunfw BY LEAF NAME (ADR-010 packaging finding), so the
// child must inherit whatever search path let the daemon itself link. Both
// platforms' variables are listed rather than one per build tag, because the
// forwarding rule is identical and a per-OS split is how one of them gets
// forgotten — which is exactly what happened: only the macOS variable was
// forwarded, and on Linux the child died with "Couldn't find or load
// libkrunfw.so.5". Only variables actually set are passed on.
var libLoaderPathVars = []string{
	"DYLD_FALLBACK_LIBRARY_PATH", // macOS
	"LD_LIBRARY_PATH",            // Linux
}

// childEnv builds the child's minimal environment. lookup and findLibDirs are
// injected so the rule is testable without mutating the process environment or
// depending on what this machine happens to have installed.
//
// Forwarding alone was not enough. Nobody exports a loader path on a normal
// install, and Homebrew's /opt/homebrew/lib is not on macOS's default fallback
// search path — so every VM child on a stock Mac died with "Couldn't find or
// load libkrunfw.5.dylib". Where the variable is unset, the directories that
// actually hold libkrunfw are appended; where it is set, the operator's value
// comes first, so a locally built libkrunfw is never silently overridden by a
// system copy. Refs: MGIT-61.15, ADR-010
func childEnv(lookup func(string) string, findLibDirs func() []string) []string {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, key := range libLoaderPathVars {
		value := lookup(key)
		if key == loaderPathVar() {
			value = strings.Join(append(splitNonEmpty(value), findLibDirs()...), ":")
		}
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// splitNonEmpty splits a colon-separated search path, dropping empty entries.
func splitNonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ":")
}

// loaderPathVar is the dynamic loader's search-path variable for this
// platform.
func loaderPathVar() string {
	if runtime.GOOS == "darwin" {
		return "DYLD_FALLBACK_LIBRARY_PATH"
	}
	return "LD_LIBRARY_PATH"
}

// libkrunfwSearchDirs are the install prefixes checked for libkrunfw, in
// preference order. They are the package managers' defaults: Homebrew on both
// architectures, and the usual system prefixes on Linux where libkrun is
// almost always built from source (no current Ubuntu release packages it).
// /usr/local/lib64 is listed because it is where a from-source Linux install
// actually lands — libkrunfw's Makefile puts a 64-bit build in $(PREFIX)/lib64
// — and Ubuntu's ld.so.conf does not cover it, so nothing else would.
var libkrunfwSearchDirs = []string{
	"/opt/homebrew/lib",
	"/usr/local/lib64",
	"/usr/local/lib",
	"/usr/lib",
	"/usr/lib64",
	"/usr/lib/" + runtime.GOARCH + "-linux-gnu",
}

// libkrunfwDirs returns the standard install directories that actually
// contain libkrunfw on this machine.
func libkrunfwDirs() []string { return libkrunfwDirsIn(libkrunfwSearchDirs) }

// libkrunfwDirsIn returns those of dirs holding a libkrunfw shared library.
// Directories that do not have it are dropped rather than padding the
// loader's search path with places it would only waste time in.
func libkrunfwDirsIn(dirs []string) []string {
	pattern := "libkrunfw.so*"
	if runtime.GOOS == "darwin" {
		pattern = "libkrunfw*.dylib"
	}
	var found []string
	for _, dir := range dirs {
		if hits, err := filepath.Glob(filepath.Join(dir, pattern)); err == nil && len(hits) > 0 {
			found = append(found, dir)
		}
	}
	return found
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
