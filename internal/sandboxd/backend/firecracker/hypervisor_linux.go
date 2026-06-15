//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// guestVsockCID is the guest-side context ID for the control plane. The
// host addresses the guest at this CID over the per-VM unix socket.
const guestVsockCID = 3

// kvmDevice is the KVM character device probed by the fail-closed
// availability check. A var (not a const) only so that fail-closed path
// is testable; it is never reassigned in production.
var kvmDevice = "/dev/kvm"

// nopLogger is a silenced logrus entry handed to the Firecracker SDK,
// whose WithLogger API is logrus-typed. The SDK's own diagnostics are
// discarded so they never reach the daemon's streams; mgit logging is
// slog. Shared across VMs — it is stateless. (logrus is sandbox-scoped
// per APPROVED-PACKAGES.md §2a.)
var nopLogger = func() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}()

// newPlatformHypervisor returns the Firecracker-on-KVM implementation.
// It fails closed when the firecracker binary or /dev/kvm is absent, so
// a missing VMM never silently degrades isolation (ADR-005, SEC-04).
// Refs: FR-17.15
func newPlatformHypervisor(bin string) (microvm.Hypervisor, error) {
	if bin == "" {
		bin = "firecracker"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: firecracker binary: %w", model.ErrSandboxBackendUnavailable, err)
	}
	if _, err := os.Stat(kvmDevice); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", model.ErrSandboxBackendUnavailable, kvmDevice, err)
	}
	return &fcHypervisor{bin: resolved}, nil
}

// fcHypervisor builds Firecracker VMs. The pure-Go SDK drives the VMM
// over its unix-socket HTTP API, so no CGO is involved.
type fcHypervisor struct{ bin string }

// vmPaths locates every per-VM host artifact under the sandbox state
// dir (the dir that holds the COW overlay). Keeping the API socket,
// vsock socket, and console there makes teardown one RemoveAll with no
// host residue (FR-17.19).
type vmPaths struct{ socket, vsock, console string }

func sandboxPaths(overlayPath string) vmPaths {
	dir := filepath.Dir(overlayPath)
	return vmPaths{
		socket:  filepath.Join(dir, "firecracker.sock"),
		vsock:   filepath.Join(dir, "vsock.sock"),
		console: filepath.Join(dir, "console.log"),
	}
}

// buildConfig translates the hypervisor-agnostic VMConfig into a
// Firecracker machine configuration: the pinned image as a read-only
// root device on vda, the per-VM writable COW overlay on vdb, and the
// vsock control plane. Firecracker has no virtiofs, so the worktree is
// NOT directory-shared here; worktree delivery to the guest is the
// guest-fs stage (MGIT-11.6). Refs: FR-17.3, FR-17.17, NFR-17.4
func buildConfig(cfg microvm.VMConfig, p vmPaths) fc.Config {
	drives := []models.Drive{
		{
			DriveID:      fc.String("rootfs"),
			PathOnHost:   fc.String(cfg.RootfsPath),
			IsRootDevice: fc.Bool(true),
			IsReadOnly:   fc.Bool(cfg.RootfsReadOnly),
		},
		{
			DriveID:      fc.String("overlay"),
			PathOnHost:   fc.String(cfg.OverlayPath),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(false),
		},
	}
	out := fc.Config{
		SocketPath:      p.socket,
		KernelImagePath: cfg.KernelPath,
		KernelArgs:      cfg.Cmdline,
		Drives:          drives,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  fc.Int64(int64(cfg.CPUs)),
			MemSizeMib: fc.Int64(int64(cfg.MemoryMB)),
			Smt:        fc.Bool(false),
		},
	}
	if cfg.VsockEnabled {
		out.VsockDevices = []fc.VsockDevice{{ID: "vsock0", Path: p.vsock, CID: guestVsockCID}}
	}
	return out
}

// CreateVM validates the configuration and constructs the machine. The
// VMM process is tied to a detached lifetime context so the guest
// outlives the request that launched it; Stop cancels it.
func (h *fcHypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	p := sandboxPaths(cfg.OverlayPath)
	fcfg := buildConfig(cfg, p)
	if err := fcfg.Validate(); err != nil {
		return nil, fmt.Errorf("firecracker config invalid: %w", err)
	}
	console, err := os.OpenFile(p.console, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // manager-owned dir
	if err != nil {
		return nil, fmt.Errorf("firecracker console: %w", err)
	}

	// The VMM process is bound to a detached lifetime context so the
	// guest outlives the launching request; cancel is the lifetime handle.
	vmmCtx, cancel := context.WithCancel(context.Background())
	cmd := fc.VMCommandBuilder{}.WithBin(h.bin).WithSocketPath(p.socket).
		WithStdout(console).WithStderr(console).Build(vmmCtx)

	machine, err := fc.NewMachine(vmmCtx, fcfg,
		fc.WithProcessRunner(cmd), fc.WithLogger(nopLogger))
	if err != nil {
		cancel()
		_ = console.Close()
		return nil, fmt.Errorf("firecracker new machine: %w", err)
	}
	return &fcVM{machine: machine, cancel: cancel, console: console}, nil
}

// fcVM adapts a Firecracker machine to the manager's lifecycle seam.
type fcVM struct {
	machine *fc.Machine
	cancel  context.CancelFunc // cancels the VMM's detached lifetime
	console *os.File
}

// teardown cancels the VMM lifetime and closes the console capture.
func (v *fcVM) teardown() {
	v.cancel()
	_ = v.console.Close()
}

// Start boots the guest. The VMM runs under the detached lifetime
// context; the passed ctx bounds only the start handshake.
func (v *fcVM) Start(ctx context.Context) error {
	if err := v.machine.Start(ctx); err != nil {
		v.teardown()
		return fmt.Errorf("firecracker start: %w", err)
	}
	return nil
}

// Stop halts the guest and reaps the VMM process. A graceful shutdown
// (CtrlAltDel) is attempted first unless force is set; either way the
// VMM is stopped and the lifetime context canceled so no process leaks.
func (v *fcVM) Stop(ctx context.Context, force bool) error {
	defer v.teardown()
	if !force {
		_ = v.machine.Shutdown(ctx)
	}
	if err := v.machine.StopVMM(); err != nil {
		return fmt.Errorf("firecracker stop: %w", err)
	}
	return nil
}
