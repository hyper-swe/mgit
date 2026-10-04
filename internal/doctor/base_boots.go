package doctor

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// BaseShape is the shape of this repository's registered guest base, as the
// daemon's boot resolves it: a directory (what `sandbox base from` composes)
// or a kernel plus a rootfs image (what `sandbox image add` registers).
type BaseShape struct {
	Name       string // the base's name in images.lock
	KernelPath string // "" when the base names no kernel
	RootfsPath string
	RootIsDir  bool
}

// BaseBootsCheck reports whether the VMM this host's daemon links can boot
// the shape of this repository's registered guest base.
//
// Each backend boots exactly one shape. libkrun boots a directory (libkrunfw
// carries the kernel, and the root is shared over virtio-fs), while
// firecracker and vzf boot a kernel and an ext4 rootfs image. Every other
// row can read ok on a host where the pair does not match: the released
// v0.6.8 on a stock ubuntu-latest linked firecracker, `sandbox base from`
// registered a directory, doctor found no known-bad condition, and every
// exec failed on `failed to stat kernel image path`. Refs: MGIT-230.4
type BaseBootsCheck struct {
	// VMM asks this host's daemon which VMM it links (the daemon/vmm probe).
	VMM func(ctx context.Context) (DaemonVMM, error)
	// Inspect resolves the registered base the way the daemon's boot does.
	Inspect func() (BaseShape, error)
}

// Name implements Check.
func (BaseBootsCheck) Name() string { return "base/boots" }

// Run implements Check.
func (c BaseBootsCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-230.4"}
	v, err := c.VMM(ctx)
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not ask this host's sandbox daemon which VMM it links, so whether it can boot the registered base is unknown"
		return r
	}
	base, err := c.Inspect()
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not resolve this repository's guest base, so whether the daemon can boot it is unknown"
		return r
	}
	switch v.Report.VMM {
	case model.BackendLibkrun:
		bootsDirectory(&r, base)
	case model.BackendKVM, model.BackendVZF:
		bootsImage(&r, vmmName(v.Report.VMM), base)
	default:
		r.Status = StatusNotChecked
		r.Reason = fmt.Sprintf("the daemon links %q, a VMM this row does not know the boot shape of", v.Report.VMM)
		r.Summary = "whether the daemon can boot the registered base is unknown: " + r.Reason
	}
	return r
}

// bootsDirectory is the libkrun verdict: the guest root must be a directory.
func bootsDirectory(r *Result, base BaseShape) {
	if base.RootIsDir {
		r.Status = StatusOK
		r.Summary = fmt.Sprintf("the daemon links libkrun, which boots a directory, and the guest base %q is one: %s",
			base.Name, base.RootfsPath)
		return
	}
	r.Status = StatusFailed
	r.Summary = fmt.Sprintf("the daemon links libkrun, which boots a directory guest root, but the guest base %q "+
		"is a kernel + rootfs image (%s): no guest can boot", base.Name, base.RootfsPath)
	r.Remedy = "compose a directory base for libkrun: `mgit sandbox base from` (no reference: the base this " +
		"release was tested with) or `mgit sandbox base from <oci-image>`; details: docs/INSTALL-SANDBOX.md"
}

// bootsImage is the firecracker and vzf verdict: a kernel and a rootfs image
// file.
func bootsImage(r *Result, vmm string, base BaseShape) {
	if base.KernelPath != "" && !base.RootIsDir {
		r.Status = StatusOK
		r.Summary = fmt.Sprintf("the daemon links %s, which boots a kernel + ext4 rootfs image, and the guest base %q "+
			"is one: kernel %s, rootfs %s", vmm, base.Name, base.KernelPath, base.RootfsPath)
		return
	}
	shape := "a directory"
	if !base.RootIsDir {
		shape = "a rootfs with no kernel"
	}
	r.Status = StatusFailed
	r.Summary = fmt.Sprintf("the daemon links %s, which boots a kernel + ext4 rootfs image, but the guest base %q "+
		"is %s (%s): no guest can boot", vmm, base.Name, shape, base.RootfsPath)
	r.Remedy = fmt.Sprintf("either run the daemon from the release archive, which links libkrun and boots a "+
		"directory base, or keep %s and install an image it can boot: "+
		"`mgit sandbox image install --from <dir-or-url>`; details: docs/INSTALL-SANDBOX.md", vmm)
}

// vmmName names a backend the way a reader knows it.
func vmmName(vmm string) string {
	switch vmm {
	case model.BackendKVM:
		return "firecracker (kvm)"
	case model.BackendVZF:
		return "Virtualization.framework (vzf)"
	}
	return vmm
}
