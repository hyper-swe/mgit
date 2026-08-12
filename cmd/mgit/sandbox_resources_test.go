package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/service"
)

// TestSandboxLaunch_ResourceFlags_ReachTheLaunchOptions verifies the declared
// caps parsed from the CLI travel on the launch the client sends.
// Refs: R-H212
func TestSandboxLaunch_ResourceFlags_ReachTheLaunchOptions(t *testing.T) {
	tests := []struct {
		name                        string
		args                        []string
		wantCPUs, wantMem, wantDisk int
	}{
		{
			name:     "all_three_declared",
			args:     []string{"--cpus", "4", "--memory-mb", "6144", "--disk-quota-mb", "20480"},
			wantCPUs: 4, wantMem: 6144, wantDisk: 20480,
		},
		{
			name:    "memory_only",
			args:    []string{"--memory-mb", "4096"},
			wantMem: 4096,
		},
		{
			name: "none_declared_stays_zero_so_policy_defaults_apply",
			args: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSandboxClient{}
			args := append([]string{"launch", "--task-id", "MGIT-95", "--worktree", "/work/a",
				"--image", "base@sha256:" + strings.Repeat("c", 64)}, tt.args...)
			_, err := runSandbox(okConnect(fake), args...)
			require.NoError(t, err)
			require.NotNil(t, fake.launched)
			assert.Equal(t, tt.wantCPUs, fake.launched.CPUs)
			assert.Equal(t, tt.wantMem, fake.launched.MemoryMB)
			assert.Equal(t, tt.wantDisk, fake.launched.DiskQuotaMB)
		})
	}
}

// TestSandboxLaunch_ResourceFlags_Documented verifies the flags are visible in
// help — a declarable cap nobody can discover is the gap this closes.
// Refs: R-H212
func TestSandboxLaunch_ResourceFlags_Documented(t *testing.T) {
	out, err := runSandbox(okConnect(&fakeSandboxClient{}), "launch", "--help")
	require.NoError(t, err)
	for _, flag := range []string{"--cpus", "--memory-mb", "--disk-quota-mb"} {
		assert.Contains(t, out, flag)
	}
	assert.Contains(t, out, "refused", "help states an over-large request is refused, not clamped")
}

// TestWorkCmd_ResourceFlags_ParsedIntoOptions verifies `mgit work` — the verb
// that actually starts an agent on a task — carries the same declarable caps.
// A flag only on `sandbox launch` would never reach the lane that starts
// agents with `work --sandbox`. Refs: R-H212, MGIT-34
func TestWorkCmd_ResourceFlags_ParsedIntoOptions(t *testing.T) {
	var got workOptions
	cmd := newWorkCmd(func(_ context.Context, _ *App, opts workOptions) error {
		got = opts
		return nil
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"wt", "--task-id", "MGIT-95",
		"--cpus", "6", "--memory-mb", "8192", "--disk-quota-mb", "30720"})

	// Run from a real initialized repo. `work` calls openAppFromCwd() BEFORE the
	// injected callback, so without a store here the callback never fires and
	// the assertions below read a zero struct that asserts nothing. An earlier
	// version of this test relied on the developer's own working tree happening
	// to be an mgit repo: green on a maintainer's machine, red on a fresh clone
	// and in CI.
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	require.NoError(t, cmd.Execute(), out.String())
	assert.Equal(t, 6, got.Resources.CPUs)
	assert.Equal(t, 8192, got.Resources.MemoryMB)
	assert.Equal(t, 30720, got.Resources.DiskQuotaMB)
}

// TestWorkSetup_DeclaredResources_ReachTheLaunch verifies the caps `mgit work`
// parsed are the ones its sandbox leg launches with. Refs: R-H212, MGIT-34
func TestWorkSetup_DeclaredResources_ReachTheLaunch(t *testing.T) {
	adder := &fakeWorktreeAdder{}
	fake := &fakeSandboxClient{}
	opts := workOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-95", LaunchSandbox: true,
		Image:     "base@sha256:" + strings.Repeat("d", 64),
		Network:   model.NetworkModeNone,
		Resources: resourceFlags{CPUs: 4, MemoryMB: 6144, DiskQuotaMB: 20480},
	}
	_, _, err := runWorkSetup(t, adder, opts, okConnect(fake))
	require.NoError(t, err)
	require.NotNil(t, fake.launched, "work --sandbox launches the bound sandbox")
	assert.Equal(t, 4, fake.launched.CPUs)
	assert.Equal(t, 6144, fake.launched.MemoryMB)
	assert.Equal(t, 20480, fake.launched.DiskQuotaMB)
}

// TestSandboxStatus_ReportsEffectiveCaps verifies the effective ceiling is
// READABLE: an agent must be able to check its cap instead of inferring it
// from a build that died. Refs: R-H212
func TestSandboxStatus_ReportsEffectiveCaps(t *testing.T) {
	fake := &fakeSandboxClient{statusInfo: &model.SandboxInfo{
		ID: "01JSB", TaskID: "MGIT-95", State: model.StateRunning,
		CPUs: 4, MemoryMB: 6144, DiskQuotaMB: 20480,
	}}
	out, err := runSandbox(okConnect(fake), "status", "MGIT-95")
	require.NoError(t, err)
	assert.Contains(t, out, "6144 MB memory")
	assert.Contains(t, out, "4 vCPU")
	assert.Contains(t, out, "20480 MB disk")
}

// TestSandboxExec_MemoryExhaustionExit_NamesTheCap verifies the diagnostic at
// the POINT OF FAILURE: a signal-death exit prints the sandbox's memory cap
// and tells the caller to declare more rather than reshape the workload. That
// missing sentence is what let a sandbox's ceiling nearly become permanent
// bundler config in a customer's repository. Refs: R-H212
func TestSandboxExec_MemoryExhaustionExit_NamesTheCap(t *testing.T) {
	tests := []struct {
		name       string
		exitCode   int
		wantAdvice bool
	}{
		{name: "v8_abort_134", exitCode: 134, wantAdvice: true},
		{name: "oom_kill_137", exitCode: 137, wantAdvice: true},
		{name: "signaled_child_minus_one", exitCode: -1, wantAdvice: true},
		{name: "ordinary_build_failure_1", exitCode: 1, wantAdvice: false},
		{name: "test_failure_2", exitCode: 2, wantAdvice: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSandboxClient{execCode: tt.exitCode, statusInfo: &model.SandboxInfo{
				ID: "01JSB", TaskID: "MGIT-95", WorktreePath: "/work/a",
				State: model.StateRunning, CPUs: 2, MemoryMB: 2048,
			}}
			out, err := runSandbox(okConnect(fake), "exec", "--task-id", "MGIT-95", "--", "npm", "run", "build")
			require.Error(t, err, "a non-zero guest exit propagates")
			if !tt.wantAdvice {
				assert.NotContains(t, out, "capped at")
				return
			}
			assert.Contains(t, out, "capped at 2048 MB of memory")
			assert.Contains(t, out, "--memory-mb")
			assert.Contains(t, out, "do not reshape the build to fit the sandbox")
		})
	}
}

// TestRunExec_MemoryExhaustionExit_NamesTheCap verifies `mgit run` — the verb
// an agent's shell is actually routed through — carries the same advisory,
// using the sandbox it already resolved. Refs: R-H212, MGIT-11.11.5
func TestRunExec_MemoryExhaustionExit_NamesTheCap(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeSandboxClient{execCode: 134, listResult: []model.SandboxInfo{{
		ID: "01JSB", TaskID: "MGIT-95", WorktreePath: canonicalPath(dir),
		State: model.StateRunning, CPUs: 2, MemoryMB: 2048,
	}}}
	cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--", "npm", "run", "build"})
	require.Error(t, cmd.Execute())
	assert.Contains(t, out.String(), "capped at 2048 MB of memory")
	assert.Contains(t, out.String(), "mgit sandbox launch --task-id MGIT-95")
}

// --- CLI -> daemon -> service, over the real control protocol --------------
//
// The unit tests above stop at the client interface. MGIT-77, MGIT-83 and
// MGIT-65 were each green at that layer and broken for a user, so the two
// tests below drive the actual cobra command through a real unix-socket
// daemon into a real SandboxService: a flag that never serialized, or a
// refusal that never crossed the wire, fails here.

// resourceTestPolicy is a host policy reader with an explicit per-sandbox
// memory maximum.
type resourceTestPolicy struct{ p model.SandboxPolicy }

func (r resourceTestPolicy) Load(context.Context) (model.SandboxPolicy, error) { return r.p, nil }

// resourceTestEvents accepts every audit event (the trail itself is covered
// by the store tests).
type resourceTestEvents struct{}

func (resourceTestEvents) AppendSandboxEvent(context.Context, *model.SandboxEvent) error { return nil }

// recordingManager captures the launch options the backend would turn into a
// VMConfig, and never boots anything.
type recordingManager struct{ lastOpts model.SandboxLaunchOptions }

func (m *recordingManager) Launch(_ context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	m.lastOpts = opts
	return &model.SandboxInfo{ID: opts.SandboxID, TaskID: opts.TaskID, WorktreePath: opts.WorktreePath,
		Backend: model.BackendKVM, State: model.StateRunning, MemoryMB: opts.MemoryMB}, nil
}
func (m *recordingManager) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (m *recordingManager) Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error) {
	return &model.ExecResult{}, nil
}
func (m *recordingManager) Stop(context.Context, string, bool) error   { return nil }
func (m *recordingManager) Remove(context.Context, string, bool) error { return nil }
func (m *recordingManager) Resolve(context.Context, string) (*model.SandboxInfo, error) {
	return nil, nil
}

// startResourceDaemon runs a real daemon over a real socket, dispatching to a
// real SandboxService bound to policy p, and returns a connector for the CLI.
func startResourceDaemon(t *testing.T, p model.SandboxPolicy, mgr model.SandboxManager) connectFunc {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host IPC (unix socket + peer-cred auth) is not implemented on Windows")
	}
	svc, err := service.NewSandboxService(mgr, resourceTestEvents{}, resourceTestPolicy{p: p},
		func() time.Time { return time.Now().UTC() },
		func() (string, error) { return "01JXSB" + strings.Repeat("0", 21), nil })
	require.NoError(t, err)

	// Keep the socket path short: sun_path is ~104 bytes.
	dir, err := os.MkdirTemp("", "mgs")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")

	d, err := sandboxd.New(sandboxd.Config{
		SocketPath: socket, Manager: mgr, Clock: time.Now, IdleGrace: time.Hour,
		PollInterval: 20 * time.Millisecond, Service: svc,
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitForDaemonSocket(t, socket)
	return func(context.Context) (sandboxClient, error) {
		return sandboxd.NewClient(socket, time.Now), nil
	}
}

// waitForDaemonSocket blocks until the daemon's socket accepts a connection.
func waitForDaemonSocket(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			if conn, dErr := net.Dial("unix", path); dErr == nil {
				_ = conn.Close()
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("daemon socket %s never became dialable", path)
}

// TestSandboxLaunchCLI_MemoryFlag_ReachesTheDaemonAndTheBackend verifies the
// whole path a user exercises: `mgit sandbox launch --memory-mb` -> control
// protocol -> daemon -> service -> the launch options the backend receives.
// Refs: R-H212, MGIT-88
func TestSandboxLaunchCLI_MemoryFlag_ReachesTheDaemonAndTheBackend(t *testing.T) {
	mgr := &recordingManager{}
	connect := startResourceDaemon(t, model.DefaultSandboxPolicy(), mgr)

	out, err := runSandbox(connect, "launch", "--task-id", "MGIT-95", "--worktree", t.TempDir(),
		"--image", "base@sha256:"+strings.Repeat("e", 64), "--memory-mb", "6144", "--cpus", "4")
	require.NoError(t, err, out)

	// Registration is lazy; the effective cap is already reported.
	statusOut, err := runSandbox(connect, "status", "MGIT-95")
	require.NoError(t, err)
	assert.Contains(t, statusOut, "6144 MB memory")

	// And it is the value the backend is asked to launch with (the VMConfig
	// input), not just something the daemon echoed back.
	cl, err := connect(context.Background())
	require.NoError(t, err)
	_, err = cl.Exec(context.Background(), "MGIT-95", model.ExecRequest{Command: []string{"true"}},
		&bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, 6144, mgr.lastOpts.MemoryMB, "the declared memory reaches the backend launch")
	assert.Equal(t, 4, mgr.lastOpts.CPUs)
}

// TestSandboxLaunchCLI_OverPolicyMaximum_RefusedWithTheLimit verifies the
// refusal itself survives the wire: the CLI reports the per-sandbox limit and
// the policy field that set it, and nothing is launched. Refs: R-H212
func TestSandboxLaunchCLI_OverPolicyMaximum_RefusedWithTheLimit(t *testing.T) {
	p := model.DefaultSandboxPolicy()
	p.MaxMemoryMB = 4096
	mgr := &recordingManager{}
	connect := startResourceDaemon(t, p, mgr)

	out, err := runSandbox(connect, "launch", "--task-id", "MGIT-95", "--worktree", t.TempDir(),
		"--image", "base@sha256:"+strings.Repeat("f", 64), "--memory-mb", "8192")
	require.Error(t, err, "an over-bound launch is refused, not clamped")
	combined := out + err.Error()
	assert.Contains(t, combined, "8192", "the refusal names what was asked for")
	assert.Contains(t, combined, "4096", "the refusal names the limit")
	assert.Contains(t, combined, "max_memory_mb", "the refusal names the policy field that set it")
	assert.Zero(t, mgr.lastOpts.MemoryMB, "nothing was launched")
}

// TestRunExec_GuestStoppedAnswering_NamesTheCap verifies the ceiling is also
// named on the transport-failure path. Live evidence (MGIT-95): a guest driven
// into real memory exhaustion loses its supervisor to the kernel OOM killer,
// so the host sees a dropped exec channel and then a refused vsock dial — no
// exit code at all. Without this, the most severe form of the failure R-H212
// is about is the one that explains itself least. Refs: R-H212
func TestRunExec_GuestStoppedAnswering_NamesTheCap(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeSandboxClient{execErr: errGuestGone, listResult: []model.SandboxInfo{{
		ID: "01JSB", TaskID: "MGIT-95", WorktreePath: canonicalPath(dir),
		State: model.StateRunning, CPUs: 2, MemoryMB: 2048,
	}}}
	cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--", "npm", "run", "build"})
	require.Error(t, cmd.Execute())
	assert.Contains(t, out.String(), "guest stopped answering")
	assert.Contains(t, out.String(), "capped at 2048 MB of memory")
}

// errGuestGone models the transport failure a vanished guest produces.
var errGuestGone = errors.New("sandbox exec: read frame: EOF")
