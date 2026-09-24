//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Against the real, linked libkrun: the probe child (this test binary,
// re-exec'd through TestMain) makes a context, which is what makes libkrun
// load libkrunfw, and reports where both resolved. No VM is started, so this
// needs no hypervisor access. Refs: MGIT-229
func TestDescribe_RealLibrary_FindsLibkrunAndItsKernelLibrary(t *testing.T) {
	r := Describe(t.Context(), os.Args[0])
	t.Logf("REAL VM-CHILD PROBE: %+v", r)
	require.True(t, r.CanBoot(), "problems: %q", r.Problems)
	for _, lib := range r.Libraries {
		assert.NotEmpty(t, lib.Path, "%s resolved nowhere", lib.Name)
		assert.FileExists(t, lib.Path)
	}
}

// NEGATIVE CONTROL: the same probe, run WITHOUT the VM child's loader search
// path, must be able to fail. On a stock Mac libkrunfw is then unreachable;
// on a from-source Linux install libkrun itself is. If this passed, the probe
// above could not tell a working install from a broken one. Refs: MGIT-229
func TestDescribe_RealLibrary_WithoutTheLoaderPath_ReportsAProblem(t *testing.T) {
	r := describeWithEnv(t.Context(), os.Args[0], []string{"PATH=/usr/bin:/bin"})
	t.Logf("NEGATIVE CONTROL PROBE: %+v", r)
	assert.False(t, r.CanBoot(), "a probe that cannot fail proves nothing: %+v", r)
}

// describeWithEnv runs the probe with a caller-chosen environment, for the
// negative control above.
func describeWithEnv(ctx context.Context, exePath string, env []string) model.VMMReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := probeCmd(ctx, exePath)
	cmd.Env = env
	return runProbe(cmd)
}
