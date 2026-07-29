package libkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
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
