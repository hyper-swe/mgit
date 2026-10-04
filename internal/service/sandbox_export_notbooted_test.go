package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Measured on the released v0.6.8 after a failed first boot: `mgit sandbox
// export` said "no sandbox backend available on this platform: task "WALK-1"
// has a registered sandbox that has not booted", while the daemon's own log
// said vmm_linked vmm=kvm. What is missing is a booted VM, not a backend. The
// refusal names the not-booted state and the next step, and nothing it did
// not verify. Refs: MGIT-232
func TestSandboxService_ExportArtifact_Unbooted_SaysNotBootedNotBackendUnavailable(t *testing.T) {
	svc := newSvc(t, &exportingManager{}, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-232", "/work/a"))
	require.NoError(t, err)

	_, err = svc.ExportArtifact(context.Background(), "MGIT-232",
		model.ArtifactExportRequest{GuestPath: "out", HostPath: filepath.Join(t.TempDir(), "x")})

	require.ErrorIs(t, err, model.ErrSandboxNotRunning)
	assert.False(t, errors.Is(err, model.ErrSandboxBackendUnavailable), "the backend is available: %v", err)
	assert.NotContains(t, err.Error(), "no sandbox backend available")
	assert.Contains(t, err.Error(), "has not booted")
	assert.Contains(t, err.Error(), "run something in it first", "the next step is named")
}
