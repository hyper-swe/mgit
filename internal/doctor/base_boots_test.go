package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// The incident this row exists for, measured on the released v0.6.8 installed
// by install.sh on a stock ubuntu-latest: the archive's daemon linked
// firecracker, `sandbox base from` registered a DIRECTORY base, and doctor
// printed ok for base/currency, base/release and daemon/loads and ended "No
// check found a known-bad condition", while every exec failed on `failed to
// stat kernel image path`. A doctor that reads clean on a host where no guest
// can ever boot is worse than no row. Each backend boots one shape, so the
// verdict is the pair. Refs: MGIT-230.4
func TestBaseBootsCheck_TheLinkedVMMAgainstTheBaseShape(t *testing.T) {
	dir := BaseShape{Name: "base", RootfsPath: "/cache/bases/0f1e", RootIsDir: true}
	img := BaseShape{Name: "base", KernelPath: "/img/vmlinux", RootfsPath: "/img/rootfs.ext4"}
	noKernel := BaseShape{Name: "base", RootfsPath: "/img/rootfs.ext4"}
	tests := []struct {
		name    string
		vmm     string
		base    BaseShape
		want    Status
		summary []string // each must appear in the row's summary
		remedy  []string // each must appear in the remedy; none means no remedy
	}{
		{"firecracker_with_a_directory_base", model.BackendKVM, dir, StatusFailed,
			[]string{"firecracker", "kernel + ext4 rootfs image", "directory", "/cache/bases/0f1e"},
			[]string{"release archive", "mgit sandbox image install --from"}},
		{"vzf_with_a_directory_base", model.BackendVZF, dir, StatusFailed,
			[]string{"vzf", "kernel + ext4 rootfs image", "directory"},
			[]string{"mgit sandbox image install --from"}},
		{"firecracker_with_no_kernel", model.BackendKVM, noKernel, StatusFailed,
			[]string{"firecracker", "no kernel"},
			[]string{"mgit sandbox image install --from"}},
		{"libkrun_with_an_image_base", model.BackendLibkrun, img, StatusFailed,
			[]string{"libkrun", "directory", "kernel + rootfs image", "/img/rootfs.ext4"},
			[]string{"mgit sandbox base from"}},
		{"libkrun_with_a_directory_base", model.BackendLibkrun, dir, StatusOK,
			[]string{"libkrun", "directory", "/cache/bases/0f1e"}, nil},
		{"firecracker_with_an_image_base", model.BackendKVM, img, StatusOK,
			[]string{"firecracker", "/img/vmlinux", "/img/rootfs.ext4"}, nil},
		{"vzf_with_an_image_base", model.BackendVZF, img, StatusOK,
			[]string{"vzf", "/img/vmlinux"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := BaseBootsCheck{
				VMM: func(context.Context) (DaemonVMM, error) {
					return DaemonVMM{Path: "/opt/mgit/bin/mgit-sandboxd", Report: model.VMMReport{VMM: tt.vmm}}, nil
				},
				Inspect: func() (BaseShape, error) { return tt.base, nil },
			}
			r := c.Run(context.Background())
			assert.Equal(t, "base/boots", r.Name)
			assert.Equal(t, "MGIT-230.4", r.Incident, "each row carries its incident")
			assert.Equal(t, tt.want, r.Status, "summary: %s", r.Summary)
			for _, s := range tt.summary {
				assert.Contains(t, r.Summary, s)
			}
			for _, s := range tt.remedy {
				assert.Contains(t, r.Remedy, s)
			}
			if len(tt.remedy) == 0 {
				assert.Empty(t, r.Remedy, "an ok row carries no remedy")
			}
		})
	}
}

// Cannot tell is a verdict of its own, never an ok: no daemon to ask, no base
// registered, or a VMM this row does not know the shape of.
func TestBaseBootsCheck_CannotTellIsNotChecked(t *testing.T) {
	dir := BaseShape{Name: "base", RootfsPath: "/cache/bases/0f1e", RootIsDir: true}
	vmm := func(name string, err error) func(context.Context) (DaemonVMM, error) {
		return func(context.Context) (DaemonVMM, error) {
			return DaemonVMM{Path: "/d", Report: model.VMMReport{VMM: name}}, err
		}
	}
	tests := []struct {
		name    string
		vmm     func(context.Context) (DaemonVMM, error)
		inspect func() (BaseShape, error)
		reason  string
	}{
		{"no_daemon_answer", vmm("", errors.New("mgit-sandboxd binary not found")),
			func() (BaseShape, error) { return dir, nil }, "mgit-sandboxd binary not found"},
		{"no_base_registered", vmm(model.BackendLibkrun, nil),
			func() (BaseShape, error) { return BaseShape{}, errors.New("no guest base registered") }, "no guest base registered"},
		{"an_unknown_vmm", vmm("hyperkit", nil),
			func() (BaseShape, error) { return dir, nil }, "hyperkit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := BaseBootsCheck{VMM: tt.vmm, Inspect: tt.inspect}.Run(context.Background())
			assert.Equal(t, StatusNotChecked, r.Status)
			assert.Contains(t, r.Reason, tt.reason)
			assert.NotEmpty(t, r.Summary)
		})
	}
}
