package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

func vmmProbe(v DaemonVMM, err error) func(context.Context) (DaemonVMM, error) {
	return func(context.Context) (DaemonVMM, error) { return v, err }
}

// daemon/vmm exists because a daemon that LOADS is not yet a daemon that can
// BOOT: libkrun loads libkrunfw (the guest kernel) lazily, so a missing
// libkrunfw passes daemon/loads and fails only at the first launch.
// Refs: MGIT-229
func TestDaemonVMMCheck_Verdicts(t *testing.T) {
	bootable := model.VMMReport{VMM: model.BackendLibkrun, Libraries: []model.VMMLibrary{
		{Name: "libkrun", Path: "/opt/mgit/bin/lib/libkrun.so.1"},
		{Name: "libkrunfw", Path: "/opt/mgit/bin/lib/libkrunfw.so.5"},
	}}
	noKernel := model.VMMReport{VMM: model.BackendLibkrun,
		Libraries: []model.VMMLibrary{{Name: "libkrun", Path: "/opt/mgit/bin/lib/libkrun.so.1"}, {Name: "libkrunfw"}},
		Problems:  []string{"libkrun did not load libkrunfw, the library that carries the guest kernel"}}
	tests := []struct {
		name        string
		goos        string
		probe       func(context.Context) (DaemonVMM, error)
		wantStatus  Status
		wantSummary []string
		wantRemedy  []string
		notInRemedy []string
	}{
		{
			name:        "bootable_names_the_vmm_and_every_library_path",
			goos:        "linux",
			probe:       vmmProbe(DaemonVMM{Path: "/opt/mgit/bin/mgit-sandboxd", Report: bootable}, nil),
			wantStatus:  StatusOK,
			wantSummary: []string{"libkrun", "/opt/mgit/bin/lib/libkrun.so.1", "/opt/mgit/bin/lib/libkrunfw.so.5"},
		},
		{
			name:        "linux_missing_kernel_library_fails_and_says_reinstall_never_brew",
			goos:        "linux",
			probe:       vmmProbe(DaemonVMM{Path: "/opt/mgit/bin/mgit-sandboxd", Report: noKernel}, nil),
			wantStatus:  StatusFailed,
			wantSummary: []string{"no guest can boot", "libkrunfw"},
			wantRemedy:  []string{"lib/", "release archive"},
			notInRemedy: []string{"brew"},
		},
		{
			name:        "darwin_missing_kernel_library_fails_with_the_brew_commands",
			goos:        "darwin",
			probe:       vmmProbe(DaemonVMM{Path: "/opt/homebrew/bin/mgit-sandboxd", Report: noKernel}, nil),
			wantStatus:  StatusFailed,
			wantSummary: []string{"no guest can boot"},
			wantRemedy:  []string{"brew install libkrun", "lib/", "release archive"},
		},
		{
			name: "firecracker_problem_names_the_prerequisite",
			goos: "linux",
			probe: vmmProbe(DaemonVMM{Path: "/usr/local/bin/mgit-sandboxd", Report: model.VMMReport{
				VMM: model.BackendKVM, Problems: []string{"the firecracker binary is not on PATH"}}}, nil),
			wantStatus: StatusFailed,
			wantRemedy: []string{"firecracker"},
		},
		{
			name:        "probe_error_is_not_checked_never_ok",
			goos:        "linux",
			probe:       vmmProbe(DaemonVMM{}, errors.New("this mgit-sandboxd predates --vmm")),
			wantStatus:  StatusNotChecked,
			wantSummary: []string{"could not ask"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := DaemonVMMCheck{Probe: tt.probe, GOOS: tt.goos}.Run(context.Background())
			assert.Equal(t, "daemon/vmm", r.Name)
			assert.Equal(t, "MGIT-229", r.Incident)
			assert.Equal(t, tt.wantStatus, r.Status, "summary: %s", r.Summary)
			for _, want := range tt.wantSummary {
				assert.Contains(t, r.Summary, want)
			}
			for _, want := range tt.wantRemedy {
				assert.Contains(t, r.Remedy, want)
			}
			for _, not := range tt.notInRemedy {
				assert.NotContains(t, r.Remedy, not)
			}
			if tt.wantStatus == StatusNotChecked {
				assert.NotEmpty(t, r.Reason)
			}
		})
	}
}
