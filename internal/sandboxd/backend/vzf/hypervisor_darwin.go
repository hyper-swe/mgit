//go:build darwin && cgo

package vzf

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"

	"github.com/hyper-swe/mgit/internal/guestboot"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// stagingDirName is the per-VM SEC-03 staging tree vzf shares over virtiofs
// instead of the live worktree: worktree files + the private .mgit, with any
// in-worktree store dropped and escaping symlinks rejected. It lives in the
// sandbox state dir (the overlay's dir), so the manager's teardown RemoveAll
// clears it with the rest of the per-sandbox host state. Refs: SEC-03
const stagingDirName = "worktree-staging"

// newPlatformHypervisor returns the Virtualization.framework
// implementation. Requires a binary signed with the
// com.apple.security.virtualization entitlement at runtime; creation
// succeeds here, entitlement failures surface at CreateVM/Start. The
// registry binds each running VM to the host dialer (FR-17.16).
// Refs: FR-17.15, ADR-005
func newPlatformHypervisor(reg *liveVMs) (microvm.Hypervisor, error) {
	return &vzHypervisor{reg: reg}, nil
}

// vzHypervisor translates vmConfig into vz configuration objects and
// publishes each started VM into the live-VM registry the dialer reads.
type vzHypervisor struct{ reg *liveVMs }

// CreateVM builds and validates the full VM configuration: Linux boot
// loader, read-only rootfs + COW overlay block devices, virtiofs
// worktree share at the identical path, vsock device, optional NAT
// NIC, and the memory balloon. Refs: FR-17.3, FR-17.7, FR-17.17, NFR-17.4
func (h *vzHypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	// Tell the guest to mount the virtiofs worktree share (tag) at the
	// worktree's identical path (FR-17.3, guestboot, MGIT-11.6.5).
	cmdline := cfg.Cmdline
	if cfg.WorktreePath != "" {
		cmdline = guestboot.AppendCmdline(cmdline, guestboot.WorktreeMount{
			Path: cfg.WorktreePath, FSType: "virtiofs", Source: cfg.WorktreeTag,
		})
	}
	bootLoader, err := vz.NewLinuxBootLoader(cfg.KernelPath, vz.WithCommandLine(cmdline))
	if err != nil {
		return nil, fmt.Errorf("vz boot loader: %w", err)
	}

	vmCfg, err := vz.NewVirtualMachineConfiguration(bootLoader,
		uint(cfg.CPUs), uint64(cfg.MemoryMB)<<20) //nolint:gosec // OK: CPUs validated non-negative by SandboxLaunchOptions
	if err != nil {
		return nil, fmt.Errorf("vz vm configuration: %w", err)
	}

	storage, err := storageDevices(cfg)
	if err != nil {
		return nil, err
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration(storage)

	share, err := worktreeShare(cfg)
	if err != nil {
		return nil, err
	}
	vmCfg.SetDirectorySharingDevicesVirtualMachineConfiguration(
		[]vz.DirectorySharingDeviceConfiguration{share})

	if err := attachAuxDevices(vmCfg, cfg); err != nil {
		return nil, err
	}

	if ok, err := vmCfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vz configuration invalid: %w", err)
	}

	vm, err := vz.NewVirtualMachine(vmCfg)
	if err != nil {
		return nil, fmt.Errorf("vz new vm: %w", err)
	}
	return &vzVM{vm: vm, id: cfg.SandboxID, reg: h.reg}, nil
}

// attachAuxDevices wires the vsock control plane (FR-17.27), the NAT
// NIC (only when the network mode is not "none", FR-17.7), and the
// memory balloon (NFR-17.4) onto the VM configuration.
func attachAuxDevices(vmCfg *vz.VirtualMachineConfiguration, cfg microvm.VMConfig) error {
	if cfg.VsockEnabled {
		sock, err := vz.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			return fmt.Errorf("vz vsock device: %w", err)
		}
		vmCfg.SetSocketDevicesVirtualMachineConfiguration(
			[]vz.SocketDeviceConfiguration{sock})
	}
	if cfg.AttachNIC {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return fmt.Errorf("vz nat attachment: %w", err)
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if err != nil {
			return fmt.Errorf("vz network device: %w", err)
		}
		vmCfg.SetNetworkDevicesVirtualMachineConfiguration(
			[]*vz.VirtioNetworkDeviceConfiguration{nic})
	}
	if cfg.BalloonEnabled {
		balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
		if err != nil {
			return fmt.Errorf("vz balloon device: %w", err)
		}
		vmCfg.SetMemoryBalloonDevicesVirtualMachineConfiguration(
			[]vz.MemoryBalloonDeviceConfiguration{balloon})
	}
	return nil
}

// storageDevices builds the immutable rootfs and writable overlay
// block devices.
func storageDevices(cfg microvm.VMConfig) ([]vz.StorageDeviceConfiguration, error) {
	rootfs, err := vz.NewDiskImageStorageDeviceAttachment(cfg.RootfsPath, cfg.RootfsReadOnly)
	if err != nil {
		return nil, fmt.Errorf("vz rootfs attachment: %w", err)
	}
	rootfsDev, err := vz.NewVirtioBlockDeviceConfiguration(rootfs)
	if err != nil {
		return nil, fmt.Errorf("vz rootfs device: %w", err)
	}
	overlay, err := vz.NewDiskImageStorageDeviceAttachment(cfg.OverlayPath, false)
	if err != nil {
		return nil, fmt.Errorf("vz overlay attachment: %w", err)
	}
	overlayDev, err := vz.NewVirtioBlockDeviceConfiguration(overlay)
	if err != nil {
		return nil, fmt.Errorf("vz overlay device: %w", err)
	}
	return []vz.StorageDeviceConfiguration{rootfsDev, overlayDev}, nil
}

// worktreeShare wires the virtiofs device that maps the worktree subtree into
// the guest at the identical path, delivering the SEC-03 quarantine.
//
// vzf shares a LIVE host directory over virtiofs — there is no copy-and-land
// image-build step like firecracker's. To still deliver the SEC-03 contract
// (worktree files + a PRIVATE sandbox-local .mgit, with the host shared store
// unreachable, any in-worktree store excluded, and escaping symlinks rejected),
// we DO NOT share the live worktree when a private store is provisioned: we
// build a STAGED copy of the worktree via the shared staging package (the same
// one firecracker packs) and share THAT staged dir instead.
//
// DESIGN TRADE-OFF (copy vs. live): a virtiofs share could in principle follow
// the guest's writes back to the live worktree, but it cannot host-side EXCLUDE
// an in-worktree .mgit/.git, REBIND .mgit to a sandbox-local store, or REJECT
// an escaping symlink before the guest follows it — all of which are SEC-03
// delivery invariants the guest (the attacker) must never get to violate.
// virtiofs has no per-entry deny or symlink-resolution-boundary control, so a
// live share cannot fail closed. A staged copy enforces every invariant
// host-side before the guest boots, at the cost of a copy on launch and a
// land-back of committed objects on exit (land is already the only
// private->shared bridge, so the guest's file edits are sandbox-local by
// design and need not flow back live). This matches firecracker's copy-and-land
// model, keeping ONE delivery semantics across backends.
//
// When PrivateStorePath is empty (no provisioner wired — tests, the documented
// pre-SEC-03 direct path) the live worktree is shared unchanged. The guest
// always mounts at cfg.WorktreePath (identical path); only the host SOURCE
// differs (staged dir vs. live worktree). Refs: SEC-03, FR-17.3, F-A/NEW-2
func worktreeShare(cfg microvm.VMConfig) (vz.DirectorySharingDeviceConfiguration, error) {
	source := cfg.WorktreePath
	if cfg.PrivateStorePath != "" {
		// Build the quarantined staging tree in the sandbox state dir (the
		// overlay's dir), cleaned by the manager's teardown RemoveAll. Build
		// fails closed (staging.ErrSymlinkEscape) on an escaping symlink, so an
		// unquarantined worktree is never shared.
		stagingDir := filepath.Join(filepath.Dir(cfg.OverlayPath), stagingDirName)
		if err := staging.Build(cfg.WorktreePath, cfg.PrivateStorePath, stagingDir); err != nil {
			return nil, fmt.Errorf("vz worktree quarantine: %w", err)
		}
		source = stagingDir
	}

	dir, err := vz.NewSharedDirectory(source, false)
	if err != nil {
		return nil, fmt.Errorf("vz shared directory: %w", err)
	}
	single, err := vz.NewSingleDirectoryShare(dir)
	if err != nil {
		return nil, fmt.Errorf("vz directory share: %w", err)
	}
	fs, err := vz.NewVirtioFileSystemDeviceConfiguration(cfg.WorktreeTag)
	if err != nil {
		return nil, fmt.Errorf("vz virtiofs device: %w", err)
	}
	fs.SetDirectoryShare(single)
	return fs, nil
}

// vzVM adapts *vz.VirtualMachine to the manager's lifecycle seam and is the
// live handle the host dialer connects through. It registers itself in the
// live-VM registry once started and deregisters on stop, so the dialer
// reaches only running VMs. Refs: FR-17.16
type vzVM struct {
	vm  *vz.VirtualMachine
	id  string
	reg *liveVMs
}

// guestConnector is satisfied by the live VM so the CGO-free dialer can
// resolve a sandbox to its framework connect.
var _ guestConnector = (*vzVM)(nil)

// Start boots the guest, then publishes it to the live-VM registry so the
// host dialer can reach its vsock channels.
func (v *vzVM) Start(_ context.Context) error {
	if err := v.vm.Start(); err != nil {
		return fmt.Errorf("vz start: %w", err)
	}
	v.reg.put(v.id, v)
	return nil
}

// Stop halts the guest and deregisters it; force uses an immediate stop,
// otherwise a graceful stop request is attempted first.
func (v *vzVM) Stop(_ context.Context, force bool) error {
	if err := v.halt(force); err != nil {
		return err
	}
	v.reg.remove(v.id)
	return nil
}

// halt performs the framework stop without touching the registry.
func (v *vzVM) halt(force bool) error {
	if !force && v.vm.CanRequestStop() {
		if ok, err := v.vm.RequestStop(); err == nil && ok {
			return nil
		}
	}
	if err := v.vm.Stop(); err != nil {
		return fmt.Errorf("vz stop: %w", err)
	}
	return nil
}

// connectGuest opens a host->guest vsock connection to a port on the
// running VM via the framework. It fails closed when the VM has no socket
// device (the vsock control plane was not attached). The returned
// *vz.VirtioSocketConnection is a net.Conn. Refs: FR-17.11, FR-17.16
func (v *vzVM) connectGuest(port uint32) (net.Conn, error) {
	devs := v.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, fmt.Errorf("%w: sandbox VM has no vsock socket device", model.ErrSandboxBackendUnavailable)
	}
	conn, err := devs[0].Connect(port)
	if err != nil {
		return nil, fmt.Errorf("vz vsock connect to guest port %d: %w", port, err)
	}
	return conn, nil
}
