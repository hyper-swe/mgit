package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rows are the host states MGIT-206 found and the ones it must not
// confuse: a daemon that loads, one the dynamic loader refuses (macOS and
// glibc spellings), one that dies for another reason, and a core-only
// install with no daemon binary at all — which is an absence, not a pass.
func TestDaemonLoadsCheck(t *testing.T) {
	tests := []struct {
		name       string
		load       DaemonLoad
		probeErr   error
		wantStatus Status
		wantIn     string
		wantRemedy string
	}{
		{"a_daemon_that_loads_is_ok",
			DaemonLoad{Path: "/opt/homebrew/bin/mgit-sandboxd", Version: "mgit-sandboxd version 0.6.6 (commit: abc, built: 2026-09-10)"},
			nil, StatusOK, "0.6.6", ""},
		{"a_missing_dylib_is_failed_and_named",
			DaemonLoad{Path: "/opt/homebrew/bin/mgit-sandboxd",
				Output:         "dyld[93889]: Library not loaded: /opt/homebrew/opt/virglrenderer/lib/libvirglrenderer.1.dylib",
				MissingLibrary: "libvirglrenderer.1.dylib",
				Remedy:         "libvirglrenderer.1.dylib is missing. mgit-sandboxd links it, so no sandbox can start"},
			nil, StatusFailed, "libvirglrenderer.1.dylib", "libvirglrenderer.1.dylib is missing"},
		{"a_missing_so_is_failed_and_named",
			DaemonLoad{Path: "/usr/local/bin/mgit-sandboxd",
				Output:         "mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file",
				MissingLibrary: "libkrun.so.1",
				Remedy:         "libkrun.so.1 is missing. mgit-sandboxd links it, so no sandbox can start"},
			nil, StatusFailed, "libkrun.so.1", "libkrun.so.1 is missing"},
		{"any_other_death_is_failed_with_its_words",
			DaemonLoad{Path: "/x/mgit-sandboxd", Output: "Segmentation fault"},
			nil, StatusFailed, "Segmentation fault", "docs/INSTALL-SANDBOX.md"},
		{"no_daemon_binary_is_NOT_a_pass",
			DaemonLoad{}, errors.New("mgit-sandboxd binary not found (install it alongside mgit or on PATH)"),
			StatusNotChecked, "not found", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DaemonLoadsCheck{Probe: func(context.Context) (DaemonLoad, error) { return tt.load, tt.probeErr }}
			got := c.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status)
			assert.Contains(t, got.Summary+got.Reason, tt.wantIn)
			assert.Equal(t, "MGIT-206", got.Incident)
			assert.Equal(t, "daemon/loads", got.Name)
			if tt.wantStatus == StatusFailed {
				assert.Contains(t, got.Summary, tt.load.Path, "the summary names the binary that was run")
				assert.Contains(t, got.Remedy, tt.wantRemedy)
			}
			if tt.wantStatus == StatusOK {
				assert.Empty(t, got.Remedy)
			}
		})
	}
}
