package main

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The daemon/vmm probe runs `mgit-sandboxd --vmm` and reads its JSON. The
// fake daemons print what a real one prints (the report shape is
// model.VMMReport's wire form) or what an older one does with an unknown
// flag (Go's flag package: "flag provided but not defined: -vmm", exit 2).
// Refs: MGIT-229
func TestProbeDaemonVMMAt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake daemons are shell scripts")
	}
	tests := []struct {
		name       string
		body       string
		wantErr    string
		wantVMM    string
		wantBoot   bool
		wantKrunfw string
	}{
		{
			name:       "bootable_report",
			body:       `[ "$1" = --vmm ] || exit 9; echo '{"vmm":"libkrun","libraries":[{"name":"libkrun","path":"/l/libkrun.so.1"},{"name":"libkrunfw","path":"/l/libkrunfw.so.5"}]}'`,
			wantVMM:    model.BackendLibkrun,
			wantBoot:   true,
			wantKrunfw: "/l/libkrunfw.so.5",
		},
		{
			name:    "report_with_problems_is_still_a_report",
			body:    `echo '{"vmm":"libkrun","libraries":[{"name":"libkrun","path":"/l/libkrun.so.1"},{"name":"libkrunfw"}],"problems":["libkrun did not load libkrunfw"]}'`,
			wantVMM: model.BackendLibkrun,
		},
		{
			name:    "older_daemon_without_the_flag",
			body:    `echo "flag provided but not defined: -vmm" >&2; exit 2`,
			wantErr: "predates --vmm",
		},
		{
			name:    "daemon_that_cannot_start",
			body:    `echo "mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file" >&2; exit 127`,
			wantErr: "libkrun.so.1",
		},
		{
			name:    "garbage_is_not_a_report",
			body:    `echo hello`,
			wantErr: "not a VMM report",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := probeDaemonVMMAt(context.Background(), fakeDaemon(t, tt.body))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantVMM, got.Report.VMM)
			assert.Equal(t, tt.wantBoot, got.Report.CanBoot())
			for _, l := range got.Report.Libraries {
				if l.Name == "libkrunfw" {
					assert.Equal(t, tt.wantKrunfw, l.Path)
				}
			}
			assert.NotEmpty(t, got.Path)
		})
	}
}
