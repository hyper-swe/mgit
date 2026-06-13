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

	if cfg.VsockEnabled {
		sock, err := vz.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("vz vsock device: %w", err)
		}
		vmCfg.SetSocketDevicesVirtualMachineConfiguration(
			[]vz.SocketDeviceConfiguration{sock})
	}

	if cfg.AttachNIC {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, fmt.Errorf("vz nat attachment: %w", err)
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
		if err != nil {
			return nil, fmt.Errorf("vz network device: %w", err)
		}
		vmCfg.SetNetworkDevicesVirtualMachineConfiguration(
			[]*vz.VirtioNetworkDeviceConfiguration{nic})
	}

	if cfg.BalloonEnabled {
		balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("vz balloon device: %w", err)
		}
		vmCfg.SetMemoryBalloonDevicesVirtualMachineConfiguration(
			[]vz.MemoryBalloonDeviceConfiguration{balloon})
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

// worktreeShare exposes ONLY the worktree to the guest via virtiofs
// (FR-17.3: working-tree files only; the parent repo, shared object
// store, and host $HOME are never shared).
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
