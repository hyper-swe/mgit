package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A host with no /dev/kvm boots no guest, yet every other row read ok or ?
// while every launch failed. host/kvm opens the device read-write, the way a
// VMM does, and fails with a plain cause and remedy. Refs: MGIT-301
func TestKVMDeviceCheck_Verdicts(t *testing.T) {
	tests := []struct {
		name        string
		open        func(string) error
		wantStatus  Status
		wantSummary []string
		wantRemedy  []string
	}{
		{
			name:       "openable_is_ok_and_names_the_device",
			open:       func(string) error { return nil },
			wantStatus: StatusOK, wantSummary: []string{"/dev/kvm"},
		},
		{
			name:        "absent_fails_naming_no_kvm_device_and_how_to_get_one",
			open:        func(p string) error { return &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist} },
			wantStatus:  StatusFailed,
			wantSummary: []string{"no KVM device", "/dev/kvm", "no guest can boot"},
			wantRemedy:  []string{"nested virtualization", "bare-metal"},
		},
		{
			name:        "permission_denied_fails_and_names_the_group",
			open:        func(p string) error { return &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission} },
			wantStatus:  StatusFailed,
			wantSummary: []string{"/dev/kvm", "permission", "no guest can boot"},
			wantRemedy:  []string{"kvm group"},
		},
		{
			name:        "any_other_open_error_is_a_failure_with_the_error_not_an_ok",
			open:        func(p string) error { return &fs.PathError{Op: "open", Path: p, Err: errors.New("device busy")} },
			wantStatus:  StatusFailed,
			wantSummary: []string{"device busy"},
			wantRemedy:  []string{"dmesg"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := KVMDeviceCheck{Open: tt.open, GOOS: "linux"}.Run(context.Background())
			assert.Equal(t, "host/kvm", r.Name)
			assert.Equal(t, "MGIT-301", r.Incident)
			assert.Equal(t, tt.wantStatus, r.Status, "summary: %s", r.Summary)
			for _, w := range tt.wantSummary {
				assert.Contains(t, r.Summary, w)
			}
			for _, w := range tt.wantRemedy {
				assert.Contains(t, r.Remedy, w)
			}
			if tt.wantStatus == StatusOK {
				assert.Empty(t, r.Remedy)
			}
		})
	}
}

// The default probe opens the real path read-write; pointed at a regular file
// it must open, and at a missing path it must report absence.
func TestOpenKVMDevice_OpensReadWriteAndReportsAbsence(t *testing.T) {
	f := t.TempDir() + "/dev-kvm"
	assert.NoError(t, os.WriteFile(f, nil, 0o600))
	assert.NoError(t, openReadWrite(f))
	assert.ErrorIs(t, openReadWrite(f+"-missing"), fs.ErrNotExist)
}

// Off Linux the row says it does not apply; it is never an ok over an
// unchecked device.
func TestKVMDeviceCheck_OffLinuxIsNotCheckedNeverOk(t *testing.T) {
	r := KVMDeviceCheck{GOOS: "darwin", Open: func(string) error { t.Fatal("must not probe"); return nil }}.Run(context.Background())
	assert.Equal(t, StatusNotChecked, r.Status)
	assert.NotEmpty(t, r.Reason)
}
