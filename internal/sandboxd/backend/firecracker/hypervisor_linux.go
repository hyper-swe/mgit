//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// guestVsockCID is the guest-side context ID for the control plane. The
// host addresses the guest at this CID over the per-VM unix socket.
const guestVsockCID = 3

// worktreeDevice is the guest block device the worktree ext4 image
// appears as: drives attach in buildConfig order (rootfs=vda, overlay=vdb,
// worktree=vdc), so the worktree is the third virtio-blk device. The guest
// mounts it at the worktree's identical path (guestboot). Refs: MGIT-11.6.5
const worktreeDevice = "/dev/vdc"

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
//
// extIface is the host external interface open mode NATs the guest out
// through. When empty, the host's default-route interface is auto-detected
// best-effort; detection failure leaves it empty so the default-safe modes
// (none/allowlist) still work and only open mode fails closed (TapPlan.validate).
// Refs: FR-17.15, FR-17.7
func newPlatformHypervisor(bin, extIface string) (microvm.Hypervisor, error) {
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
	if extIface == "" {
		// Best-effort: open mode needs an upstream; the safe modes do not.
		extIface, _ = defaultRouteIface()
	}
	return &fcHypervisor{bin: resolved, mkfs: mke2fsExecRunner{}, net: ipRunner{}, extIface: extIface}, nil
}

// fcHypervisor builds Firecracker VMs. The pure-Go SDK drives the VMM
// over its unix-socket HTTP API, so no CGO is involved.
type fcHypervisor struct {
	bin  string
	mkfs mkfsRunner // builds the worktree ext4 image (copy-and-land, ADR-005)
	net  NetRunner  // execs host tap + firewall setup/teardown (ip/iptables)
	// extIface is the host external interface NATed in open mode. Empty for
	// the default-safe modes (none/allowlist), where no NAT is configured;
	// open mode requires the daemon to supply it (TapPlan fails closed if
	// unset). Refs: FR-17.7
	extIface string
}

// mke2fsExecRunner is the real worktree-image builder: it execs mke2fs to
// create+populate an ext4 filesystem. mke2fs -d needs no root. The binary
// must be on PATH (e2fsprogs; commonly /usr/sbin). Refs: MGIT-11.6.4
type mke2fsExecRunner struct{}

func (mke2fsExecRunner) run(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "mke2fs", args...).CombinedOutput() //nolint:gosec // argv built from manager-owned paths, no shell
	if err != nil {
		return fmt.Errorf("mke2fs: %w: %s", err, out)
	}
	return nil
}

// buildConfig translates the hypervisor-agnostic VMConfig into a
// Firecracker machine configuration: the pinned image as a read-only
// root device on vda, the per-VM writable COW overlay on vdb, and the
// vsock control plane. Firecracker has no virtiofs, so the worktree is
// delivered copy-and-land as a writable ext4 image on vdc when
// worktreeImg is set (ADR-005, MGIT-11.6.4); the guest mounts it at the
// worktree's identical path (MGIT-11.6.5). Refs: FR-17.3, FR-17.17, NFR-17.4
func buildConfig(cfg microvm.VMConfig, p vmPaths, worktreeImg string) fc.Config {
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
	if worktreeImg != "" {
		drives = append(drives, models.Drive{
			DriveID:      fc.String("worktree"),
			PathOnHost:   fc.String(worktreeImg),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(false), // the guest edits the worktree copy
		})
	}
	// When the worktree image is attached (vdc), tell the guest to mount
	// it at the worktree's identical path (copy-and-land, MGIT-11.6.5).
	kernelArgs := cfg.Cmdline
	if worktreeImg != "" {
		kernelArgs = guestboot.AppendCmdline(kernelArgs, guestboot.WorktreeMount{
			Path: cfg.WorktreePath, FSType: "ext4", Source: worktreeDevice,
		})
	}

	out := fc.Config{
		SocketPath:      p.socket,
		KernelImagePath: cfg.KernelPath,
		KernelArgs:      kernelArgs,
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
	// Attach a NIC in every mode except none (FR-17.7). The guest gets a
	// static IP on its per-sandbox /30 with the host tap as gateway and the
	// host resolver (on the gateway) as its only nameserver — so DNS is
	// host-side and, in allowlist mode, the tap firewall gives the guest no
	// route except to the proxy and resolver. The host-side tap + firewall
	// are created in CreateVM before boot. Refs: FR-17.7, FR-17.8, SEC-04
	if cfg.AttachNIC {
		gw, _, guestNet := subnetFor(cfg.SandboxID)
		out.NetworkInterfaces = fc.NetworkInterfaces{{
			StaticConfiguration: &fc.StaticNetworkConfiguration{
				HostDevName: egress.TapName(cfg.SandboxID),
				MacAddress:  guestMAC(cfg.SandboxID),
				IPConfiguration: &fc.IPConfiguration{
					IPAddr:      guestNet,
					Gateway:     net.IP(gw.AsSlice()),
					Nameservers: []string{gw.String()},
				},
			},
		}}
	}
	return out
}

// CreateVM validates the configuration and constructs the machine. The
// VMM process is tied to a detached lifetime context so the guest
// outlives the request that launched it; Stop cancels it.
func (h *fcHypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	stateDir := filepath.Dir(cfg.OverlayPath)
	p := sandboxPaths(stateDir)

	// Copy-and-land worktree delivery (ADR-005): pack the worktree into a
	// per-VM ext4 image in the state dir (cleaned by teardown's RemoveAll),
	// attached as a writable drive. mke2fs is a quick local op, so it runs
	// on a background context (the Hypervisor seam has none).
	var worktreeImg string
	if cfg.WorktreePath != "" {
		worktreeImg = filepath.Join(stateDir, worktreeImageName)
		if err := buildWorktreeImage(context.Background(), h.mkfs, cfg.WorktreePath, worktreeImg, 0); err != nil {
			return nil, fmt.Errorf("firecracker worktree delivery: %w", err)
		}
	}

	fcfg := buildConfig(cfg, p, worktreeImg)
	if err := fcfg.Validate(); err != nil {
		return nil, fmt.Errorf("firecracker config invalid: %w", err)
	}

	// Create the host tap + per-mode firewall BEFORE the VMM starts (the
	// NIC's HostDevName must already exist, and allowlist mode must be
	// default-deny before the guest can send a packet). none mode is a
	// no-op. Stored on the VM so teardown removes it (no residue). The tap
	// is host-side only — the egress proxy/DNS bind its gateway IP.
	// Refs: FR-17.7, FR-17.8, SEC-04
	var tapPlan *egress.TapPlan
	if cfg.AttachNIC {
		plan := tapPlanFor(cfg.SandboxID, cfg.NetworkMode, h.extIface)
		if err := applyTapPlan(context.Background(), h.net, plan); err != nil {
			return nil, fmt.Errorf("firecracker network setup: %w", err)
		}
		tapPlan = &plan
	}

	console, err := os.OpenFile(p.console, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // manager-owned dir
	if err != nil {
		h.removeTap(tapPlan)
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
		h.removeTap(tapPlan)
		return nil, fmt.Errorf("firecracker new machine: %w", err)
	}
	return &fcVM{machine: machine, cancel: cancel, console: console, net: h.net, tapPlan: tapPlan}, nil
}

// removeTap best-effort tears down a sandbox's host tap + firewall, guarding
// the none-mode (nil) case.
func (h *fcHypervisor) removeTap(plan *egress.TapPlan) {
	if plan != nil {
		removeTapPlan(context.Background(), h.net, *plan)
	}
}

// fcVM adapts a Firecracker machine to the manager's lifecycle seam.
type fcVM struct {
	machine *fc.Machine
	cancel  context.CancelFunc // cancels the VMM's detached lifetime
	console *os.File
	net     NetRunner       // host network runner, for tap teardown
	tapPlan *egress.TapPlan // the applied tap plan; nil in none mode
}

// PeerIdentity reports the host-observed vsock peer identity. Over
// Firecracker the guest's well-known context ID is guestVsockCID; the
// per-VM unix socket (keyed by sandbox ID) is the actual transport
// discriminator, so the daemon's channel authorization keys on the
// addressed sandbox and confirms this bound peer. The CID is host-observed,
// never guest-asserted (SEC-05). Refs: FR-17.27, SEC-10
func (v *fcVM) PeerIdentity() string {
	return fmt.Sprintf("cid:%d", guestVsockCID)
}

// teardown cancels the VMM lifetime, closes the console capture, and removes
// the host tap + firewall so no network residue remains (FR-17.19).
func (v *fcVM) teardown() {
	v.cancel()
	_ = v.console.Close()
	if v.tapPlan != nil {
		removeTapPlan(context.Background(), v.net, *v.tapPlan)
	}
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
