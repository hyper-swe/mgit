package libkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// Wiring-level tests: the guest dialers, the manager construction, and the
// child's audit sink. These are the seams the daemon actually consumes, so a
// break here is invisible to the unit tests above but fatal in production.
// Refs: FR-17.5, FR-17.11, FR-17.8, MGIT-61.9

// listenGuestPort binds a unix listener where the backend expects a sandbox's
// vsock port socket, standing in for the socket libkrun's child creates.
func listenGuestPort(t *testing.T, workDir, sandboxID string, port uint32) net.Listener {
	t.Helper()
	stateDir := microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	ln, err := net.Listen("unix", vsockSocketPath(stateDir, port))
	if err != nil {
		t.Fatalf("listen guest port %d: %v", port, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("guest"))
			_ = c.Close()
		}
	}()
	return ln
}

func TestGuestDialers_ReachTheirOwnPortSocket(t *testing.T) {
	tests := []struct {
		name string
		port uint32
		dial func(workDir string) microvm.GuestDialer
	}{
		{
			name: "exec_dialer",
			port: microvm.GuestExecPort,
			dial: func(workDir string) microvm.GuestDialer { return newGuestDialer(workDir) },
		},
		{
			// The land channel is a DIFFERENT port on the same transport;
			// a dialer that ignored its port would silently land over exec.
			name: "land_dialer",
			port: microvm.GuestLandPort,
			dial: NewLandDialer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := shortTempDir(t)
			const sandboxID = "sbx-dial"
			listenGuestPort(t, workDir, sandboxID, tt.port)

			conn, err := tt.dial(workDir).DialGuest(context.Background(), sandboxID)
			if err != nil {
				t.Fatalf("DialGuest: %v", err)
			}
			defer func() { _ = conn.Close() }()

			buf := make([]byte, 5)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(buf) != "guest" {
				t.Errorf("read %q, want %q", buf, "guest")
			}
		})
	}
}

func TestGuestDialer_NoSuchSandbox_ErrorNamesPortAndSandbox(t *testing.T) {
	_, err := newGuestDialer(shortTempDir(t)).DialGuest(context.Background(), "sbx-missing")
	if err == nil {
		t.Fatal("dialing a sandbox with no VM must fail")
	}
	// The diagnosis matters: "connection refused" alone does not say WHICH
	// sandbox or WHICH channel failed.
	for _, want := range []string{"sbx-missing", "1024"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewManager_WiresTheBackendAndItsDialer(t *testing.T) {
	mgr, err := NewManager(Config{
		WorkDir: shortTempDir(t),
		Resolve: func(string) (microvm.ImagePaths, error) { return microvm.ImagePaths{}, nil },
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		Clock:   func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	list, err := mgr.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a fresh manager reports %d sandboxes, want 0", len(list))
	}
}

func TestNewManager_RejectsAMissingLogger(t *testing.T) {
	// NewHypervisor refuses without a logger, so the failure must surface
	// from NewManager rather than producing a manager with a nil hypervisor.
	_, err := NewManager(Config{WorkDir: shortTempDir(t)})
	if err == nil {
		t.Fatal("NewManager with no logger must fail")
	}
}

func TestNewHypervisor_ResolvesTheBinaryItWillReExec(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hv, err := NewHypervisor(logger)
	if err != nil {
		t.Fatalf("NewHypervisor: %v", err)
	}
	// Resolved ONCE at construction: if the binary cannot re-exec itself no
	// VM can ever start, so that must fail here, not at the first launch.
	if hv.exePath == "" {
		t.Error("hypervisor did not resolve its own executable path")
	}
	if hv.spawn == nil {
		t.Error("hypervisor has no spawner")
	}

	if _, err := NewHypervisor(nil); err == nil {
		t.Error("NewHypervisor(nil logger) must fail")
	}
}

func TestLogAuditor_RecordsEveryFieldOfAnEgressDecision(t *testing.T) {
	// The child cannot reach the daemon's durable audit store, so this log
	// IS the audit trail for a libkrun sandbox's egress (MGIT-61.9). A
	// dropped field there is a hole in the record.
	var buf bytes.Buffer
	aud := logAuditor{logger: slog.New(slog.NewJSONHandler(&buf, nil))}

	err := aud.AppendEgressRecord(context.Background(), &model.EgressRecord{
		SandboxID: "sbx-audit", TaskID: "MGIT-61.9", Decision: model.EgressDeny,
		Protocol: "tcp", DestHost: "example.com", DestIP: "93.184.216.34",
		DestPort: 443, Rule: "raw-ip not allowlisted",
	})
	if err != nil {
		t.Fatalf("AppendEgressRecord: %v", err)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("audit line is not JSON: %v (%s)", err, buf.String())
	}
	for k, want := range map[string]any{
		"event": "egress_record", "sandbox_id": "sbx-audit", "task_id": "MGIT-61.9",
		"decision": model.EgressDeny, "protocol": "tcp", "dest_host": "example.com",
		"dest_ip": "93.184.216.34", "dest_port": float64(443), "rule": "raw-ip not allowlisted",
	} {
		if rec[k] != want {
			t.Errorf("audit field %s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestNetGateway_DialGuestPort_FailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		waitFor time.Duration
		wantErr string
	}{
		{name: "port_below_range", port: 0, waitFor: time.Second, wantErr: "out of range"},
		{name: "port_above_range", port: 70000, waitFor: time.Second, wantErr: "out of range"},
		// Until the guest has transmitted, its return address is unknown and
		// outbound frames have nowhere to go — say so instead of hanging.
		{name: "guest_has_not_spoken_yet", port: 22, waitFor: 50 * time.Millisecond,
			wantErr: "guest network is not up yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			gw, err := bindNetGateway(filepath.Join(dir, proxySocketName), &stubAuthorizer{}, nil, nil)
			if err != nil {
				t.Fatalf("bindNetGateway: %v", err)
			}
			t.Cleanup(func() { _ = gw.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), tt.waitFor)
			defer cancel()
			conn, err := gw.DialGuestPort(ctx, tt.port)
			if err == nil {
				_ = conn.Close()
				t.Fatal("expected a failure")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			if !errors.Is(err, model.ErrSandboxBackendUnavailable) {
				t.Errorf("error %v does not wrap ErrSandboxBackendUnavailable", err)
			}
		})
	}
}

func TestCheckSocketPathLen_NamesTheSocketAndTheRemedy(t *testing.T) {
	if err := checkSocketPathLen("net backing socket", "/short/path.sock"); err != nil {
		t.Fatalf("a short path must pass: %v", err)
	}
	err := checkSocketPathLen("vsock socket", "/"+strings.Repeat("d", maxUnixSocketPath))
	if err == nil {
		t.Fatal("an over-long path must fail")
	}
	// bind(2) would say only "invalid argument"; the whole point of checking
	// at resolve time is to say what and what to do.
	for _, want := range []string{"vsock socket", "shorter sandbox state directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// SEC-09 ONE-WAY PORT PUBLISHING. netGateway.DialGuestPort cannot serve this
// on libkrun: the gateway lives in the VM CHILD process, so the daemon holds
// no reference to it. libkrun's own inbound primitive is used instead —
// krun_add_vsock_port2(..., listen=true) makes libkrun LISTEN on a host unix
// socket and forward into a guest vsock port, which is exactly firecracker's
// shape, so the existing publisher and the guest's AF_VSOCK->TCP bridge are
// reused unchanged. Refs: SEC-09, FR-17.8, MGIT-61.13 P6

func TestPortDialer_ReachesAPublishedGuestPort(t *testing.T) {
	workDir := shortTempDir(t)
	const sandboxID, guestPort = "sbx-pub", 8080
	listenGuestPort(t, workDir, sandboxID, uint32(guestPort))

	conn, err := NewPortDialer(workDir).DialGuestPort(context.Background(), sandboxID, guestPort)
	if err != nil {
		t.Fatalf("DialGuestPort: %v", err)
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 5)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read from published port: %v", err)
	}
	if string(buf) != "guest" {
		t.Errorf("read %q, want %q", buf, "guest")
	}
}

func TestPortDialer_FailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr string
	}{
		{name: "port_below_range", port: 0, wantErr: "out of range"},
		{name: "port_above_range", port: 70000, wantErr: "out of range"},
		// A port the VM never published has no libkrun listener behind it;
		// the host must not silently reach anything else.
		{name: "unpublished_port", port: 9999, wantErr: "sbx-none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPortDialer(shortTempDir(t)).
				DialGuestPort(context.Background(), "sbx-none", tt.port)
			if err == nil {
				t.Fatal("expected a failure")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// Fail-closed paths that only fire on real filesystem or resource errors.
// They matter because each one guards a launch: a silent failure here is a
// sandbox that boots without the property it was supposed to have.
// Refs: MGIT-61.9, SEC-03, SEC-10

func TestBindNetGateway_FailsClosedOnUnusablePaths(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			name: "unbindable_directory",
			path: func(t *testing.T) string {
				// A path whose parent does not exist cannot be bound.
				return filepath.Join(shortTempDir(t), "no-such-dir", proxySocketName)
			},
			wantErr: "bind gateway socket",
		},
		{
			name: "stale_path_is_a_nonempty_dir",
			path: func(t *testing.T) string {
				// A directory in the socket's place cannot be removed, so the
				// stale-socket replacement must fail rather than proceed.
				dir := shortTempDir(t)
				p := filepath.Join(dir, proxySocketName)
				if err := os.MkdirAll(filepath.Join(p, "child"), 0o750); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantErr: "clear stale gateway socket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw, err := bindNetGateway(tt.path(t), &stubAuthorizer{}, nil, nil)
			if err == nil {
				_ = gw.Close()
				t.Fatal("an unusable gateway socket path must fail the launch")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeliverWorktree_ReturnsTheWorktreeWhenNothingToStage(t *testing.T) {
	tests := []struct {
		name string
		cfg  microvm.VMConfig
		want string
	}{
		{name: "no_worktree_at_all", cfg: microvm.VMConfig{}, want: ""},
		{
			// No private store means no quarantine to build; the pre-SEC-03
			// direct path shares the worktree itself.
			name: "worktree_without_private_store",
			cfg:  microvm.VMConfig{WorktreePath: "/work/wt"},
			want: "/work/wt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deliverWorktree(tt.cfg)
			if err != nil {
				t.Fatalf("deliverWorktree: %v", err)
			}
			if got != tt.want {
				t.Errorf("host dir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeliverWorktree_PrivateStoreWithoutStateDir_FailsClosed(t *testing.T) {
	// The staged tree carries the private store, so writing it to a
	// process-relative path would put quarantined content outside the
	// sandbox's own dir and leave it behind at teardown.
	_, err := deliverWorktree(microvm.VMConfig{
		WorktreePath: "/work/wt", PrivateStorePath: "/priv",
	})
	if err == nil {
		t.Fatal("staging without a state dir must fail closed")
	}
	if !errors.Is(err, model.ErrSandboxBackendUnavailable) {
		t.Errorf("error %v does not wrap ErrSandboxBackendUnavailable", err)
	}
}

func TestVMSpec_Encode_SurfacesAWriteFailure(t *testing.T) {
	// The spec crosses a process boundary; a partial write would hand the
	// child a truncated document rather than failing the launch.
	err := validSpec(t).encode(errWriter{})
	if err == nil {
		t.Fatal("a failed spec write must surface")
	}
	if !strings.Contains(err.Error(), "encode libkrun vm spec") {
		t.Errorf("error %q does not name the operation", err)
	}
}

func TestChildPolicy_UnknownMode_RefusesRatherThanGuessing(t *testing.T) {
	// netBackingFor rejects unknown modes before a spec can reach here, so
	// this pins the remaining branch rather than a reachable state. What
	// matters is the direction of the failure: an unrecognized mode produces
	// an ERROR, never a permissive authorizer and never a nil one (which
	// would mean "no gateway" — the discard socket — and quietly change the
	// sandbox's network posture).
	spec := baseSpec("bogus-mode", shortTempDir(t))
	auth, dns, err := childPolicy(spec, testChildLogger(), testClock())
	if err == nil {
		t.Fatal("an unrecognized network mode must be refused, not guessed at")
	}
	if auth != nil || dns != nil {
		t.Error("a refused mode must yield no policy objects")
	}
}

// TestExecChild_SignalKillAndWait covers the real process-control adapter
// against a REAL child (the test binary re-execed as a sleeper), rather than
// the fake used by the lifecycle tests. Signal/Kill/Wait are the whole of how
// a VM is stopped, so a break here strands VMs rather than failing a test.
// Refs: MGIT-61.8, MGIT-61.9
func TestExecChild_SignalKillAndWait(t *testing.T) {
	tests := []struct {
		name string
		stop func(t *testing.T, c *execChild)
	}{
		{
			name: "graceful_signal",
			stop: func(t *testing.T, c *execChild) {
				if err := c.Signal(syscall.SIGTERM); err != nil {
					t.Fatalf("Signal: %v", err)
				}
			},
		},
		{
			name: "force_kill",
			stop: func(t *testing.T, c *execChild) {
				if err := c.Kill(); err != nil {
					t.Fatalf("Kill: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A trivial long-lived process stands in for a running VM child.
			cmd := exec.Command("/bin/sh", "-c", "sleep 30") //nolint:gosec // fixed argv
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			child := &execChild{cmd: cmd, handshake: r}

			tt.stop(t, child)
			code, _ := child.Wait()
			// Killed by signal reports -1 via os.ProcessState; either way the
			// process must be reaped rather than left behind.
			if code > 0 {
				t.Errorf("exit code = %d, want <= 0 for a signaled child", code)
			}
		})
	}
}

func TestWaitErrString_DistinguishesExitStatusFromRealFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		// A non-zero exit is NOT an error here: the code carries it, and
		// reporting it as a failure would make every workload exit look like
		// a VM fault.
		{name: "nil", err: nil, want: ""},
		{name: "exit_status", err: &exec.ExitError{}, want: ""},
		{name: "real_failure", err: errors.New("waitid: no child processes"), want: "waitid: no child processes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waitErrString(tt.err); got != tt.want {
				t.Errorf("waitErrString(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestSpawnChild_UnwritableConsole_FailsBeforeSpawning(t *testing.T) {
	// The console is the ONLY record of what a guest did, so a launch that
	// cannot open it must fail rather than run blind.
	dir := shortTempDir(t)
	consolePath := filepath.Join(dir, "no-such-dir", consoleLogName)
	_, err := spawnChild("/nonexistent/binary", baseSpec(model.NetworkModeNone, dir), consolePath)
	if err == nil {
		t.Fatal("an unopenable console must fail the spawn")
	}
	if !strings.Contains(err.Error(), "console") {
		t.Errorf("error %q does not name the console", err)
	}
}
