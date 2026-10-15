package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/hyper-swe/mgit/internal/model"
)

// kvmDevice is the device a Linux VMM opens to run a guest.
const kvmDevice = "/dev/kvm"

// KVMDeviceCheck reports whether this host can open /dev/kvm read-write, the
// way a VMM does. Without it no guest boots, yet every other row read ok or ?
// on a host with no KVM while each launch failed. Refs: MGIT-301
type KVMDeviceCheck struct {
	// Open opens the path read-write and closes it again; nil opens the real
	// file. A test injects the failure it wants.
	Open func(path string) error
	// GOOS is the host's OS: only Linux backends open /dev/kvm, so elsewhere
	// the row says it does not apply rather than passing.
	GOOS string
	// VMM reports which VMM this host's sandbox daemon links. Only libkrun and
	// firecracker open /dev/kvm: with no daemon, or the container backend, the
	// device is not this host's requirement and the row reads not-checked.
	VMM func(ctx context.Context) (DaemonVMM, error)
}

// Name implements Check.
func (KVMDeviceCheck) Name() string { return "host/kvm" }

// Run implements Check.
func (c KVMDeviceCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-301"}
	if c.GOOS != "linux" {
		r.Status = StatusNotChecked
		r.Reason = "only the Linux backends use " + kvmDevice
		r.Summary = "the KVM device is not needed on " + c.GOOS
		return r
	}
	v, verr := c.VMM(ctx)
	if verr != nil {
		r.Status, r.Reason = StatusNotChecked, verr.Error()
		r.Summary = "could not tell which VMM this host's sandbox daemon links, so whether it needs " + kvmDevice + " is unknown"
		return r
	}
	if v.Report.VMM != model.BackendLibkrun && v.Report.VMM != model.BackendKVM {
		r.Status = StatusNotChecked
		r.Reason = fmt.Sprintf("the sandbox daemon links %s, which does not open %s", orUnnamed(v.Report.VMM), kvmDevice)
		r.Summary = "the KVM device is not needed by this host's sandbox backend"
		return r
	}
	open := c.Open
	if open == nil {
		open = openReadWrite
	}
	err := open(kvmDevice)
	switch {
	case err == nil:
		r.Status = StatusOK
		r.Summary = kvmDevice + " opens read-write"
	case errors.Is(err, fs.ErrNotExist):
		r.Status = StatusFailed
		r.Summary = "no KVM device: " + kvmDevice + " does not exist, so no guest can boot on this host"
		r.Remedy = "Run on a machine with hardware virtualization exposed: a bare-metal host, or a VM with " +
			"nested virtualization enabled (cloud VMs usually need a bare-metal or nested-virtualization " +
			"instance type). `modprobe kvm_intel` or `modprobe kvm_amd` loads the driver where the CPU has it; " +
			"details: docs/INSTALL-SANDBOX.md"
	case errors.Is(err, fs.ErrPermission):
		r.Status = StatusFailed
		r.Summary = fmt.Sprintf("%s exists but this user cannot open it (permission denied), so no guest can boot", kvmDevice)
		r.Remedy = "Add this user to the kvm group (`sudo usermod -aG kvm $USER`), then log in again; " +
			"details: docs/INSTALL-SANDBOX.md"
	default:
		r.Status = StatusFailed
		r.Summary = fmt.Sprintf("%s could not be opened read-write: %v; no guest can boot", kvmDevice, err)
		r.Remedy = "Read `dmesg | tail` for the kernel's words about KVM; details: docs/INSTALL-SANDBOX.md"
	}
	return r
}

// openReadWrite opens path read-write and closes it.
func openReadWrite(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the fixed device path, or a test's own file
	if err != nil {
		return err
	}
	return f.Close()
}
