//go:build darwin && cgo

package vzf

import (
	"context"
	"fmt"

	"github.com/Code-Hex/vz/v3"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// newPlatformHypervisor returns the Virtualization.framework
// implementation. Requires a binary signed with the
// com.apple.security.virtualization entitlement at runtime; creation
// succeeds here, entitlement failures surface at CreateVM/Start.
// Refs: FR-17.15, ADR-005
func newPlatformHypervisor() (microvm.Hypervisor, error) {
	return &vzHypervisor{}, nil
}

// vzHypervisor translates vmConfig into vz configuration objects.
type vzHypervisor struct{}

// CreateVM builds and validates the full VM configuration: Linux boot
// loader, read-only rootfs + COW overlay block devices, virtiofs
// worktree share at the identical path, vsock device, optional NAT
// NIC, and the memory balloon. Refs: FR-17.3, FR-17.7, FR-17.17, NFR-17.4
func (h *vzHypervisor) CreateVM(cfg microvm.VMConfig) (microvm.VM, error) {
	bootLoader, err := vz.NewLinuxBootLoader(cfg.KernelPath,
		vz.WithCommandLine(cfg.Cmdline))
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
	return &vzVM{vm: vm}, nil
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

// worktreeShare wires the virtiofs device that maps the worktree subtree
// into the guest at the identical path. It only constructs the share
// device; the SEC-03 quarantine guarantees — rebinding the guest .git to
// a private sandbox-local object store, rejecting symlinks/.git pointers
// that resolve into the shared store, and mounting host-trusted paths
// read-only — are enforced at the guest-filesystem layer (MGIT-11.6,
// FR-17.3/4/14) and are NOT yet in place. No real hostile guest is driven
// against a worktree until that lands (exec/land routing is a later
// stage), so this device is presently exercised only by construction
// tests. Refs: FR-17.3, MGIT-11.6
func worktreeShare(cfg microvm.VMConfig) (vz.DirectorySharingDeviceConfiguration, error) {
	dir, err := vz.NewSharedDirectory(cfg.WorktreePath, false)
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

// vzVM adapts *vz.VirtualMachine to the manager's lifecycle seam.
type vzVM struct {
	vm *vz.VirtualMachine
}

// Start boots the guest.
func (v *vzVM) Start(_ context.Context) error {
	if err := v.vm.Start(); err != nil {
		return fmt.Errorf("vz start: %w", err)
	}
	return nil
}

// Stop halts the guest; force uses an immediate stop, otherwise a
// graceful stop request is attempted first.
func (v *vzVM) Stop(_ context.Context, force bool) error {
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
