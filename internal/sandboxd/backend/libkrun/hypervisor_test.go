package libkrun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// TestMain doubles as the re-exec child entry point: when the test binary is
// started with ChildCommand (by the real-spawn test), it runs the REAL
// ChildMain in a REAL child process — the exact dispatch cmd/mgit-sandboxd
// performs — so the process plumbing (spec on stdin, handshake on fd 3, exit
// reaping) is exercised without libkrun installed.
// It also dispatches the parent-lifeline helper roles (MGIT-103), which need
// a real three-process tree — test, spawner, VM child — to prove a SIGKILLed
// parent takes its VM with it.
// The helper roles are dispatched BEFORE ChildCommand: the lifeline helper
// child is spawned by the REAL newChildCmd, so its argv IS ChildCommand, and
// checking that first would run ChildMain instead — which on a build without
// a bootable VM exits at once and leaves a zombie that answers signal 0,
// making the orphan test pass without testing anything.
func TestMain(m *testing.M) {
	if code, isHelper := runLifelineHelper(); isHelper {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == ChildCommand {
		os.Exit(ChildMain(os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeChild scripts one child process for the lifecycle tests.
type fakeChild struct {
	hs io.Reader // scripted handshake stream

	mu       sync.Mutex
	signals  []os.Signal
	killed   bool
	exitCode int
	exitOnce sync.Once
	exited   chan struct{}
	// dieOnTerm makes SIGTERM end the process (a cooperative child); false
	// models a wedged child that only SIGKILL ends.
	dieOnTerm bool
}

func newFakeChild(handshake string) *fakeChild {
	return &fakeChild{hs: strings.NewReader(handshake), exited: make(chan struct{}), dieOnTerm: true}
}

func (c *fakeChild) Handshake() io.Reader { return c.hs }

func (c *fakeChild) Signal(sig os.Signal) error {
	c.mu.Lock()
	c.signals = append(c.signals, sig)
	die := c.dieOnTerm && sig == syscall.SIGTERM
	c.mu.Unlock()
	if die {
		c.exit()
	}
	return nil
}

func (c *fakeChild) Kill() error {
	c.mu.Lock()
	c.killed = true
	c.mu.Unlock()
	c.exit()
	return nil
}

func (c *fakeChild) exit() { c.exitOnce.Do(func() { close(c.exited) }) }

func (c *fakeChild) Wait() (int, error) {
	<-c.exited
	return c.exitCode, nil
}

func (c *fakeChild) wasKilled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killed
}

func (c *fakeChild) gotSignal(want os.Signal) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.signals {
		if s == want {
			return true
		}
	}
	return false
}

// fakeSpawner records every spawn and hands out scripted children.
type fakeSpawner struct {
	mu       sync.Mutex
	specs    []vmSpec
	consoles []string
	children []*fakeChild
	next     func() *fakeChild
	err      error
}

func (s *fakeSpawner) spawn(_ string, spec vmSpec, consolePath string) (childProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	child := s.next()
	s.specs = append(s.specs, spec)
	s.consoles = append(s.consoles, consolePath)
	s.children = append(s.children, child)
	return child, nil
}

func testHypervisor(t *testing.T, spawner *fakeSpawner) *Hypervisor {
	t.Helper()
	return &Hypervisor{
		exePath: "/fake/mgit-sandboxd",
		spawn:   spawner.spawn,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// vmCfg is a valid libkrun VMConfig: state dir short (unix socket limits),
// rootfs an existing DIRECTORY.
func vmCfg(t *testing.T, mode string) microvm.VMConfig {
	t.Helper()
	return microvm.VMConfig{
		SandboxID:      "sbx-h1",
		TaskID:         "MGIT-61.8",
		StateDir:       shortTempDir(t),
		CPUs:           2,
		MemoryMB:       1024,
		RootfsPath:     testGuestBase(t),
		RootfsReadOnly: true,
		WorktreePath:   "/work/wt",
		WorktreeTag:    "work",
		VsockEnabled:   true,
		NetworkMode:    mode,
		AttachNIC:      mode != model.NetworkModeNone,
	}
}

func TestCreateVM_RefusesWhatItCannotContain(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*microvm.VMConfig)
		wantErr string
	}{
		// SEC-03 delivery is implemented (see TestCreateVM_SEC03_*), so a
		// private store no longer refuses the launch — but a quarantine that
		// cannot be BUILT still must, rather than booting a guest with an
		// unstaged worktree.
		{name: "sec03_unstageable_worktree_fails_closed", mutate: func(c *microvm.VMConfig) {
			c.PrivateStorePath = "/some/private-store"
			c.WorktreePath = "/definitely/not/a/worktree"
		}, wantErr: "worktree quarantine"},
		{name: "no_state_dir", mutate: func(c *microvm.VMConfig) {
			c.StateDir = ""
		}, wantErr: "state dir"},
		{name: "rootfs_is_an_image_not_a_dir", mutate: func(c *microvm.VMConfig) {
			img := filepath.Join(t.TempDir(), "rootfs.ext4")
			if err := os.WriteFile(img, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.RootfsPath = img
		}, wantErr: "not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild("") }}
			h := testHypervisor(t, spawner)
			cfg := vmCfg(t, model.NetworkModeNone)
			tt.mutate(&cfg)

			_, err := h.CreateVM(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CreateVM = %v, want error mentioning %q", err, tt.wantErr)
			}
			if len(spawner.specs) != 0 {
				t.Error("a refused config must never spawn a child")
			}
		})
	}
}

func TestCreateVM_TranslatesTheIsolationContractIntoTheSpec(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)
	cfg := vmCfg(t, model.NetworkModeAllowlist)
	cfg.NetworkAllowlist = []string{"proxy.golang.org:443"}

	vm, err := h.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	spec := spawner.specs[0]
	if spec.SandboxID != cfg.SandboxID || spec.TaskID != cfg.TaskID {
		t.Errorf("identity = (%q,%q), want the config's", spec.SandboxID, spec.TaskID)
	}
	if spec.RootDir != cfg.RootfsPath || !spec.RootReadOnly {
		t.Errorf("root = (%q, ro=%v), want (%q, ro=true) (FR-17.17)", spec.RootDir, spec.RootReadOnly, cfg.RootfsPath)
	}
	if spec.WorktreePath != cfg.WorktreePath || spec.WorktreeTag != cfg.WorktreeTag {
		t.Errorf("worktree = (%q,%q), want the config's (FR-17.3)", spec.WorktreePath, spec.WorktreeTag)
	}
	if spec.ExecPath != guestInitPath {
		t.Errorf("exec = %q, want %q", spec.ExecPath, guestInitPath)
	}
	// The vsock ports are passed EXPLICITLY, not left to mgit-guest's flag
	// defaults: host and guest then read one source (microvm's constants).
	wantArgs := []string{"--vsock-port", "1024", "--land-vsock-port", "1025", "--notify-host-port", "1026"}
	if strings.Join(spec.ExecArgs, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("exec args = %v, want %v", spec.ExecArgs, wantArgs)
	}
	if len(spec.ExecEnv) == 0 {
		t.Error("guest env must be set explicitly; a nil envp makes libkrun inherit the daemon's (SEC-05)")
	}
	if len(spec.Allowlist) != 1 || spec.Allowlist[0] != "proxy.golang.org:443" {
		t.Errorf("allowlist = %v, want the policy verbatim (SEC-04)", spec.Allowlist)
	}
	if want := filepath.Join(cfg.StateDir, consoleLogName); spawner.consoles[0] != want {
		t.Errorf("console = %q, want %q (under the state dir, FR-17.19)", spawner.consoles[0], want)
	}
}

func TestKrunVM_Start_ConfigFailure_SelfCleans(t *testing.T) {
	child := newFakeChild(`{"ok":false,"error":"krun_add_net_unixgram: -22"}` + "\n")
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	err = vm.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "krun_add_net_unixgram") {
		t.Fatalf("Start = %v, want the child's configuration error", err)
	}
	// Manager.Launch never calls Stop after a failed Start: the VM must have
	// reaped its own child (no orphan process).
	if !child.wasKilled() {
		t.Error("failed Start left the child unkilled")
	}
	select {
	case <-child.exited:
	default:
		t.Error("failed Start left the child unreaped")
	}
}

func TestKrunVM_Start_ChildDiesSilently_SurfacesConsoleHint(t *testing.T) {
	child := newFakeChild("") // EOF before any handshake line
	child.exitCode = 127
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	err = vm.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "console") {
		t.Fatalf("Start = %v, want an error pointing at the console log", err)
	}
}

func TestKrunVM_Start_CtxCanceled_KillsTheChild(t *testing.T) {
	// A handshake stream that never produces a line (wedged child).
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	child := &fakeChild{hs: r, exited: make(chan struct{})}
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := vm.Start(ctx); err == nil {
		t.Fatal("Start with a wedged child must fail")
	}
	if !child.wasKilled() {
		t.Error("wedged child not killed")
	}
}

func TestKrunVM_Stop_Graceful_TermsThenReaps(t *testing.T) {
	child := newFakeChild(`{"ok":true}` + "\n")
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := vm.Stop(context.Background(), false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !child.gotSignal(syscall.SIGTERM) {
		t.Error("graceful stop must SIGTERM first")
	}
	if child.wasKilled() {
		t.Error("cooperative child must not be SIGKILLed")
	}
}

func TestKrunVM_Stop_WedgedChild_EscalatesToKill(t *testing.T) {
	child := newFakeChild(`{"ok":true}` + "\n")
	child.dieOnTerm = false // ignores SIGTERM
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The ctx deadline caps the grace period so the escalation is fast.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := vm.Stop(ctx, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !child.wasKilled() {
		t.Error("a child that ignores SIGTERM must be SIGKILLed")
	}
}

func TestKrunVM_Stop_ForceKillsImmediately(t *testing.T) {
	child := newFakeChild(`{"ok":true}` + "\n")
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := vm.Stop(context.Background(), true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !child.wasKilled() {
		t.Error("force stop must SIGKILL")
	}
	if child.gotSignal(syscall.SIGTERM) {
		t.Error("force stop must not wait on SIGTERM")
	}
}

func TestKrunVM_Stop_BeforeStartOrAfterExit_IsANoOp(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	// Never started.
	if err := vm.Stop(context.Background(), false); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := vm.Stop(context.Background(), true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Already exited and reaped.
	if err := vm.Stop(context.Background(), false); err != nil {
		t.Fatalf("Stop after exit: %v", err)
	}
}

func TestKrunVM_ConcurrentLaunches_AreIsolated(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)

	ids := []string{"sbx-a", "sbx-b", "sbx-c"}
	vms := make([]microvm.VM, 0, len(ids))
	for _, id := range ids {
		cfg := vmCfg(t, model.NetworkModeNone)
		cfg.SandboxID = id
		vm, err := h.CreateVM(cfg)
		if err != nil {
			t.Fatalf("CreateVM %s: %v", id, err)
		}
		vms = append(vms, vm)
	}
	var wg sync.WaitGroup
	for _, vm := range vms {
		wg.Add(1)
		go func(v microvm.VM) {
			defer wg.Done()
			if err := v.Start(context.Background()); err != nil {
				t.Errorf("Start: %v", err)
			}
		}(vm)
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, vm := range vms {
			_ = vm.Stop(context.Background(), true)
		}
	})

	seenDirs := map[string]bool{}
	for _, spec := range spawner.specs {
		if seenDirs[spec.StateDir] {
			t.Errorf("state dir %q shared between sandboxes (T6 cross-contamination)", spec.StateDir)
		}
		seenDirs[spec.StateDir] = true
	}
}

func TestKrunVM_PeerIdentityAndNotifySocket_AreHostDerivedPerVM(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild("") }}
	h := testHypervisor(t, spawner)
	cfg := vmCfg(t, model.NetworkModeNone)

	vm, err := h.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	pi, ok := vm.(microvm.PeerIdentifier)
	if !ok {
		t.Fatal("krunVM must report a host-observed peer identity (SEC-10)")
	}
	if want := vsockSocketPath(cfg.StateDir, microvm.GuestExecPort); pi.PeerIdentity() != want {
		t.Errorf("PeerIdentity = %q, want the per-VM exec socket path %q", pi.PeerIdentity(), want)
	}
	np, ok := vm.(microvm.NotifyListenerProvider)
	if !ok {
		t.Fatal("krunVM must expose the notify socket path (MGIT-11.10.11)")
	}
	if want := vsockSocketPath(cfg.StateDir, microvm.GuestNotifyPort); np.NotifySocketPath() != want {
		t.Errorf("NotifySocketPath = %q, want %q", np.NotifySocketPath(), want)
	}

	t.Run("no_vsock_no_identity", func(t *testing.T) {
		cfg := vmCfg(t, model.NetworkModeNone)
		cfg.VsockEnabled = false
		vm, err := h.CreateVM(cfg)
		if err != nil {
			t.Fatalf("CreateVM: %v", err)
		}
		if id := vm.(microvm.PeerIdentifier).PeerIdentity(); id != "" {
			t.Errorf("PeerIdentity without vsock = %q, want empty (manager falls back to sandbox ID)", id)
		}
		if p := vm.(microvm.NotifyListenerProvider).NotifySocketPath(); p != "" {
			t.Errorf("NotifySocketPath without vsock = %q, want empty (auto-land unwired)", p)
		}
	})
}

func TestIsGuestInitFailure_ReservedCodes(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{code: 0, want: false}, {code: 1, want: false}, {code: 3, want: false},
		{code: 125, want: true}, {code: 126, want: true}, {code: 127, want: true},
		{code: 128, want: false}, {code: -1, want: false},
	}
	for _, tt := range tests {
		if got := isGuestInitFailure(tt.code); got != tt.want {
			t.Errorf("isGuestInitFailure(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func TestNewChildCmd_StdioContractNeverInheritsTheDaemonStreams(t *testing.T) {
	dir := shortTempDir(t)
	spec := baseSpec(model.NetworkModeNone, dir)
	consolePath := filepath.Join(dir, consoleLogName)

	c, err := newChildCmd("/fake/mgit-sandboxd", spec, consolePath)
	if err != nil {
		t.Fatalf("newChildCmd: %v", err)
	}
	cmd := c.cmd
	t.Cleanup(func() { c.cleanup(); _ = c.handshake.Close(); _ = c.lifeline.Close() })

	// krun_start_enter hands the child's stdin/stdout to the guest, so none
	// of them may be the daemon's own streams (SEC-10).
	if cmd.Stdin == os.Stdin || cmd.Stdout == os.Stdout || cmd.Stderr == os.Stderr {
		t.Error("child stdio inherits a daemon stream; the guest would hold it")
	}
	if cmd.Stdout != cmd.Stderr {
		t.Error("stdout and stderr should both capture to the console file")
	}
	if len(cmd.ExtraFiles) != 2 {
		t.Errorf("ExtraFiles = %d, want the handshake pipe on fd 3 and the parent lifeline on fd 4",
			len(cmd.ExtraFiles))
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != ChildCommand {
		t.Errorf("args = %v, want [exe %s]", cmd.Args, ChildCommand)
	}
	for _, kv := range cmd.Env {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "DYLD_FALLBACK_LIBRARY_PATH=") &&
			!strings.HasPrefix(kv, envLifelineFD+"=") &&
			!strings.HasPrefix(kv, envLifelineNonce+"=") {
			t.Errorf("child env carries %q; the daemon environment must not leak toward the guest", kv)
		}
	}
	// The spec crosses on stdin, never argv (argv is world-readable via ps).
	specJSON, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read cmd stdin: %v", err)
	}
	var decoded vmSpec
	if err := json.Unmarshal(specJSON, &decoded); err != nil || decoded.SandboxID != spec.SandboxID {
		t.Errorf("stdin does not carry the spec: %v / %+v", err, decoded)
	}
}

// TestSpawnChild_RealProcess exercises the REAL spawn path end to end: the
// test binary re-execs itself (TestMain dispatches ChildCommand to the real
// ChildMain), the spec crosses on stdin, the handshake arrives on fd 3, and
// the child is reaped. Without libkrun in this build the child reports the
// actionable "rebuild with -tags libkrun" refusal — which is exactly the
// contract this test pins for the default build.
func TestSpawnChild_RealProcess_HandshakeStdinAndReap(t *testing.T) {
	if _, err := newPlatformAPI(); err == nil {
		t.Skip("libkrun-tagged build: the real-VM e2e covers the spawn path")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	spec := baseSpec(model.NetworkModeNone, dir)
	spec.RootDir = testGuestBase(t)
	consolePath := filepath.Join(dir, consoleLogName)

	child, err := spawnChild(exe, spec, consolePath)
	if err != nil {
		t.Fatalf("spawnChild: %v", err)
	}
	var hs childHandshake
	if err := json.NewDecoder(child.Handshake()).Decode(&hs); err != nil {
		t.Fatalf("read handshake from the real child: %v", err)
	}
	if hs.OK || !strings.Contains(hs.Error, "-tags libkrun") {
		t.Errorf("handshake = %+v, want the actionable no-libkrun refusal", hs)
	}
	code, _ := child.Wait()
	if code != 1 {
		t.Errorf("child exit = %d, want 1 (config failure)", code)
	}
	console, err := os.ReadFile(consolePath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("console log: %v", err)
	}
	if !strings.Contains(string(console), "krun_vm_failed") {
		t.Errorf("console log %q lacks the child's failure record", console)
	}
}

// TestKrunVM_ExitClassification pins how a stopped VM is reported. The child's
// exit code IS the guest workload's exit code — EXCEPT when the boot never
// reached the guest (a late handshake error) or when the code is one libkrun's
// in-guest init reserves. Conflating those would tell an agent its command
// failed when in fact the VM never ran it. Refs: MGIT-61.8, ADR-010
func TestKrunVM_ExitClassification(t *testing.T) {
	tests := []struct {
		name      string
		handshake string
		exitCode  int
		wantEvent string
	}{
		{
			// Configured, then the boot itself failed before the guest ran.
			name:      "late_handshake_error_is_a_boot_failure",
			handshake: `{"ok":true}` + "\n" + `{"error":"krun_start_enter: -22"}` + "\n",
			wantEvent: "krun_vm_bootfail",
		},
		{
			// libkrun reserves 125/126/127 for in-guest init failures.
			name:      "reserved_code_is_a_guest_init_failure",
			handshake: `{"ok":true}` + "\n",
			exitCode:  127,
			wantEvent: "krun_vm_initfail",
		},
		{
			name:      "ordinary_code_is_a_workload_exit",
			handshake: `{"ok":true}` + "\n",
			exitCode:  3,
			wantEvent: "krun_vm_exit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged strings.Builder
			child := newFakeChild(tt.handshake)
			child.exitCode = tt.exitCode
			spawner := &fakeSpawner{next: func() *fakeChild { return child }}
			h := &Hypervisor{
				exePath: "/fake/mgit-sandboxd",
				spawn:   spawner.spawn,
				logger:  slog.New(slog.NewTextHandler(&logged, nil)),
			}

			vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
			if err != nil {
				t.Fatalf("CreateVM: %v", err)
			}
			if err := vm.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			// Stop waits for the watcher to reap and classify.
			if err := vm.Stop(context.Background(), true); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if !strings.Contains(logged.String(), tt.wantEvent) {
				t.Errorf("log %q lacks event %q", logged.String(), tt.wantEvent)
			}
		})
	}
}

// TestWriteHandshake_BrokenPipe_IsLoggedNotFatal: if the parent died, the
// child must still finish its own teardown rather than dying on the report.
func TestWriteHandshake_BrokenPipe_IsLoggedNotFatal(t *testing.T) {
	var logged strings.Builder
	writeHandshake(errWriter{}, slog.New(slog.NewTextHandler(&logged, nil)), childHandshake{OK: true})
	if !strings.Contains(logged.String(), "krun_handshake_writefail") {
		t.Errorf("a failed handshake write must be logged, got %q", logged.String())
	}
}

// errWriter fails every write, standing in for a parent that has exited.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// SEC-03 DELIVERY. Until now CreateVM refused any launch carrying a private
// store, so no real sandbox could start on libkrun. These pin the delivery:
// the guest gets a STAGED tree (worktree files + the private .mgit, with the
// host's own store excluded and escaping symlinks rejected), shared at the
// identical guest path — the same contract firecracker and vzf deliver.
// Refs: SEC-03, FR-17.3, MGIT-61.13 P7

// worktreeWithStore builds a host worktree containing a file, an in-worktree
// store directory that must NOT reach the guest, and returns its path.
func worktreeWithStore(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A store dir inside the worktree: staging must drop it, or the guest
	// would get history the quarantine is supposed to withhold.
	if err := os.MkdirAll(filepath.Join(wt, ".mgit", "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".mgit", "HOST-ONLY"), []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	return wt
}

// privateStore builds a stand-in per-sandbox private store.
func privateStore(t *testing.T) string {
	t.Helper()
	priv := t.TempDir()
	if err := os.MkdirAll(filepath.Join(priv, "objects"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "HEAD"), []byte("ref: refs/heads/task/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestCreateVM_SEC03_SharesTheStagedTreeNotTheLiveWorktree(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)

	cfg := vmCfg(t, model.NetworkModeNone)
	cfg.WorktreePath = worktreeWithStore(t)
	cfg.PrivateStorePath = privateStore(t)

	vm, err := h.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM with a private store must now succeed: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	spec := spawner.specs[0]
	// What is SHARED is the staged copy under the sandbox state dir, never
	// the live worktree — a live share cannot exclude or rebind host-side.
	if spec.WorktreeHostDir == cfg.WorktreePath {
		t.Fatal("the LIVE worktree was shared; SEC-03 requires the staged tree")
	}
	if !strings.HasPrefix(spec.WorktreeHostDir, cfg.StateDir) {
		t.Errorf("staged tree %q is not under the sandbox state dir %q (teardown must reclaim it)",
			spec.WorktreeHostDir, cfg.StateDir)
	}
	// The guest still sees it at the IDENTICAL path (FR-17.3).
	if spec.WorktreePath != cfg.WorktreePath {
		t.Errorf("guest path = %q, want the identical host path %q", spec.WorktreePath, cfg.WorktreePath)
	}

	t.Run("worktree_files_present", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(spec.WorktreeHostDir, "app.go")); err != nil {
			t.Errorf("worktree file missing from the staged tree: %v", err)
		}
	})
	t.Run("private_store_laid_in_at_dot_mgit", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(spec.WorktreeHostDir, ".mgit", "HEAD")); err != nil {
			t.Errorf("private store not delivered at <worktree>/.mgit: %v", err)
		}
	})
	t.Run("host_in_worktree_store_excluded", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(spec.WorktreeHostDir, ".mgit", "HOST-ONLY")); !os.IsNotExist(err) {
			t.Error("the worktree's own .mgit reached the guest (SEC-03)")
		}
	})
	t.Run("guest_told_how_to_mount_it", func(t *testing.T) {
		// libkrun has no cmdline of ours, so the descriptor rides the env.
		var tokens string
		for _, kv := range spec.ExecEnv {
			if v, ok := strings.CutPrefix(kv, guestboot.EnvBootTokens+"="); ok {
				tokens = v
			}
		}
		got := guestboot.ParseWorktreeMount(tokens)
		if got.Path != cfg.WorktreePath || got.FSType != "virtiofs" || got.Source != cfg.WorktreeTag {
			t.Errorf("boot descriptor = %+v, want path=%q virtiofs tag=%q — without it the guest "+
				"boots with the share attached but never mounted", got, cfg.WorktreePath, cfg.WorktreeTag)
		}
	})
}

func TestCreateVM_SEC03_EscapingSymlink_FailsClosed(t *testing.T) {
	wt := worktreeWithStore(t)
	// A symlink pointing outside the worktree: the host must reject the
	// launch rather than let the guest follow it.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wt, "escape")); err != nil {
		t.Fatal(err)
	}

	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)
	cfg := vmCfg(t, model.NetworkModeNone)
	cfg.WorktreePath = wt
	cfg.PrivateStorePath = privateStore(t)

	_, err := h.CreateVM(cfg)
	if err == nil {
		t.Fatal("a worktree symlink escaping the worktree must fail the launch (SEC-03)")
	}
	if !errors.Is(err, staging.ErrSymlinkEscape) {
		t.Errorf("error %v does not wrap ErrSymlinkEscape", err)
	}
	if len(spawner.specs) != 0 {
		t.Error("a quarantine failure must never spawn a VM")
	}
}

func TestCreateVM_NoPrivateStore_SharesTheWorktreeDirectly(t *testing.T) {
	// The pre-SEC-03 direct path (no provisioner wired) must keep working:
	// nothing to stage, so the worktree itself is shared.
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)
	cfg := vmCfg(t, model.NetworkModeNone)
	cfg.WorktreePath = worktreeWithStore(t)

	vm, err := h.CreateVM(cfg)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	if spawner.specs[0].WorktreeHostDir != cfg.WorktreePath {
		t.Errorf("host dir = %q, want the worktree itself when no private store is wired",
			spawner.specs[0].WorktreeHostDir)
	}
}

// TestChildEnv_ForwardsTheLoaderSearchPath pins the rule that libkrun dlopens
// libkrunfw by leaf name, so the VM child must inherit whatever search path
// let the daemon link — on EVERY platform. Forwarding only the macOS variable
// is what made the first Linux/KVM run die with "Couldn't find or load
// libkrunfw.so.5". Refs: ADR-010, MGIT-61.13 P4
func TestChildEnv_ForwardsTheLoaderSearchPath(t *testing.T) {
	tests := []struct {
		name string
		set  map[string]string
		want []string
	}{
		{name: "nothing_set", set: map[string]string{}, want: []string{"PATH=/usr/bin:/bin"}},
		{
			name: "macos_loader_path",
			set:  map[string]string{"DYLD_FALLBACK_LIBRARY_PATH": "/opt/krunfw/lib"},
			want: []string{"PATH=/usr/bin:/bin", "DYLD_FALLBACK_LIBRARY_PATH=/opt/krunfw/lib"},
		},
		{
			name: "linux_loader_path",
			set:  map[string]string{"LD_LIBRARY_PATH": "/home/u/lk-prefix/lib64"},
			want: []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=/home/u/lk-prefix/lib64"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := childEnv(func(k string) string { return tt.set[k] }, func() []string { return nil })
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("childEnv = %v, want %v", got, tt.want)
			}
			// The daemon's own environment must never ride along wholesale.
			for _, kv := range got {
				if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "USER=") {
					t.Errorf("daemon environment leaked to the child: %q", kv)
				}
			}
		})
	}
}

// TestCreateVM_OpenMode_IsNowSupported pins the behavior change: open mode
// used to be refused because nothing could construct an authorizer for it.
// egress.OpenAuthorizer closes that, so a launch must now succeed — and open
// mode gains per-flow audit records the iptables NAT path could never produce.
// Refs: MGIT-61.9, SEC-04, FR-17.8
func TestCreateVM_OpenMode_IsNowSupported(t *testing.T) {
	spawner := &fakeSpawner{next: func() *fakeChild { return newFakeChild(`{"ok":true}` + "\n") }}
	h := testHypervisor(t, spawner)

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeOpen))
	if err != nil {
		t.Fatalf("open mode must launch now that it has an authorizer: %v", err)
	}
	if err := vm.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	if spawner.specs[0].NetworkMode != model.NetworkModeOpen {
		t.Errorf("spec network mode = %q, want open", spawner.specs[0].NetworkMode)
	}
}

// TestChildEnv_FindsLibkrunfwWhenNobodySetTheLoaderPath covers the case a
// plain install is actually in: nobody exported anything.
//
// libkrun dlopens libkrunfw BY LEAF NAME, and Homebrew's /opt/homebrew/lib is
// not on macOS's default fallback search path — so on a stock Mac the VM
// child died with "Couldn't find or load libkrunfw.5.dylib" and no VM could
// boot at all. Forwarding the variable was never enough; something has to
// SET it. Refs: MGIT-61.15, ADR-010
func TestChildEnv_FindsLibkrunfwWhenNobodySetTheLoaderPath(t *testing.T) {
	unset := func(string) string { return "" }
	found := func() []string { return []string{"/opt/homebrew/lib"} }

	got := strings.Join(childEnv(unset, found), " ")

	if !strings.Contains(got, loaderPathVar()+"=/opt/homebrew/lib") {
		t.Errorf("childEnv = %q; with nothing exported it must still point the child "+
			"at the directory holding libkrunfw", got)
	}
}

// TestChildEnv_OperatorsPathComesFirst keeps an explicitly exported search
// path authoritative: someone running a locally built libkrunfw must not have
// a system copy silently preferred over it.
func TestChildEnv_OperatorsPathComesFirst(t *testing.T) {
	set := func(k string) string {
		if k == loaderPathVar() {
			return "/home/u/my-krunfw/lib"
		}
		return ""
	}
	found := func() []string { return []string{"/opt/homebrew/lib"} }

	got := strings.Join(childEnv(set, found), " ")

	want := loaderPathVar() + "=/home/u/my-krunfw/lib:/opt/homebrew/lib"
	if !strings.Contains(got, want) {
		t.Errorf("childEnv = %q, want it to contain %q", got, want)
	}
}

// TestLibkrunfwSearchDirs_CoversTheLinuxFromSourcePrefix is the Linux twin of
// the Homebrew case above, and it is not hypothetical: no Ubuntu release
// packages libkrun or libkrunfw, so every Linux install is a from-source
// `make PREFIX=/usr/local install` — and libkrunfw's own Makefile installs a
// 64-bit build into $(PREFIX)/lib64, NOT $(PREFIX)/lib. That directory is also
// absent from Ubuntu's ld.so.conf, so nothing else puts it on the loader's
// path either. Without it in this list, a Linux VM child dies with
// "Couldn't find or load libkrunfw.so.4" unless the operator happens to have
// exported LD_LIBRARY_PATH themselves. Refs: MGIT-87, MGIT-61.15
func TestLibkrunfwSearchDirs_CoversTheLinuxFromSourcePrefix(t *testing.T) {
	for _, want := range []string{"/usr/local/lib64", "/usr/local/lib"} {
		found := false
		for _, dir := range libkrunfwSearchDirs {
			if dir == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("libkrunfwSearchDirs = %v, missing %q — the prefix a from-source "+
				"libkrunfw install actually lands in on Linux", libkrunfwSearchDirs, want)
		}
	}
}

// TestLibkrunfwDirs_OnlyReturnsDirectoriesThatHaveIt guards against padding
// the loader path with directories that do not contain the library.
func TestLibkrunfwDirs_OnlyReturnsDirectoriesThatHaveIt(t *testing.T) {
	real := t.TempDir()
	empty := t.TempDir()
	name := "libkrunfw.so.5"
	if runtime.GOOS == "darwin" {
		name = "libkrunfw.5.dylib"
	}
	if err := os.WriteFile(filepath.Join(real, name), []byte("lib"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := libkrunfwDirsIn([]string{empty, real, "/no/such/dir"})

	if len(got) != 1 || got[0] != real {
		t.Errorf("libkrunfwDirsIn = %v, want only %q", got, real)
	}
}
