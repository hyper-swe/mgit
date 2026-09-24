package libkrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The loaded-image lists below are what the dynamic loader reports on each
// platform, spelled the way the loader spells them: libkrun's SONAME on
// Linux (libkrun.so.1) and install name on macOS (libkrun.1.dylib), and
// libkrunfw likewise (libkrunfw.so.5, libkrunfw.5.dylib). They come from the
// upstream builds, not from this package's matching code. Refs: MGIT-229
func TestDescribeLoaded_ReportsWhereEachLibraryResolved(t *testing.T) {
	tests := []struct {
		name         string
		loaded       []string
		netErr       error
		wantKrun     string
		wantKrunfw   string
		wantProblems int      // how many conditions stop a boot
		wantMention  []string // words the problems must carry
	}{
		{
			name:       "linux_bundled_both_loaded",
			loaded:     []string{"/usr/lib/x86_64-linux-gnu/libc.so.6", "/opt/mgit/bin/lib/libkrun.so.1", "/opt/mgit/bin/lib/libkrunfw.so.5"},
			wantKrun:   "/opt/mgit/bin/lib/libkrun.so.1",
			wantKrunfw: "/opt/mgit/bin/lib/libkrunfw.so.5",
		},
		{
			name:       "darwin_brew_both_loaded",
			loaded:     []string{"/usr/lib/libSystem.B.dylib", "/opt/homebrew/opt/libkrun/lib/libkrun.1.dylib", "/opt/homebrew/opt/libkrunfw/lib/libkrunfw.5.dylib"},
			wantKrun:   "/opt/homebrew/opt/libkrun/lib/libkrun.1.dylib",
			wantKrunfw: "/opt/homebrew/opt/libkrunfw/lib/libkrunfw.5.dylib",
		},
		{
			name:         "libkrunfw_not_loaded_is_a_named_problem",
			loaded:       []string{"/opt/mgit/bin/lib/libkrun.so.1"},
			wantKrun:     "/opt/mgit/bin/lib/libkrun.so.1",
			wantProblems: 1,
			wantMention:  []string{"libkrunfw", "/opt/mgit/bin/lib"},
		},
		{
			name:         "networking_missing_is_a_named_problem",
			loaded:       []string{"/x/libkrun.so.1", "/x/libkrunfw.so.5"},
			netErr:       errors.New("libkrun was built without networking"),
			wantKrun:     "/x/libkrun.so.1",
			wantKrunfw:   "/x/libkrunfw.so.5",
			wantProblems: 1,
			wantMention:  []string{"without networking"},
		},
		{
			name:         "libkrun_itself_not_found",
			loaded:       []string{"/usr/lib/libc.so.6"},
			wantProblems: 2,
			wantMention:  []string{"libkrun is not loaded", "libkrunfw"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := describeLoaded(tt.loaded, tt.netErr, nil)
			assert.Equal(t, model.BackendLibkrun, r.VMM)
			assert.Equal(t, tt.wantKrun, libraryPath(r, "libkrun"), "libkrun path")
			assert.Equal(t, tt.wantKrunfw, libraryPath(r, "libkrunfw"), "libkrunfw path")
			require.Len(t, r.Problems, tt.wantProblems, "problems: %q", r.Problems)
			all := strings.Join(r.Problems, "\n")
			for _, want := range tt.wantMention {
				assert.Contains(t, all, want)
			}
			assert.Equal(t, tt.wantProblems == 0, r.CanBoot())
		})
	}
}

// libkrunfw's name starts with libkrun's, so a prefix match would report
// libkrunfw's path as libkrun's and hide a missing libkrun behind it.
func TestDescribeLoaded_DoesNotMistakeLibkrunfwForLibkrun(t *testing.T) {
	r := describeLoaded([]string{"/x/libkrunfw.so.5"}, nil, nil)
	assert.Empty(t, libraryPath(r, "libkrun"))
	assert.Equal(t, "/x/libkrunfw.so.5", libraryPath(r, "libkrunfw"))
	assert.False(t, r.CanBoot())
}

func libraryPath(r model.VMMReport, name string) string {
	for _, l := range r.Libraries {
		if l.Name == name {
			return l.Path
		}
	}
	return ""
}
