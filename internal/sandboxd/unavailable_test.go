// Package sandboxd tests verify the no-backend manager per FR-17.15.
package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestUnavailableManager_RefusesEverything verifies the specified
// no-backend behavior: launches report ErrSandboxBackendUnavailable,
// nothing is listed, and addressed operations report not-found.
// Refs: FR-17.15, FR-17.20
func TestUnavailableManager_RefusesEverything(t *testing.T) {
	mgr := NewUnavailableManager("test-os")
	ctx := context.Background()

	_, err := mgr.Launch(ctx, model.SandboxLaunchOptions{})
	assert.ErrorIs(t, err, model.ErrSandboxBackendUnavailable)
	assert.ErrorContains(t, err, "test-os", "platform named for diagnostics")

	sandboxes, err := mgr.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, sandboxes)

	_, err = mgr.Exec(ctx, "01JX", model.ExecRequest{Command: []string{"true"}})
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
	assert.ErrorIs(t, mgr.Stop(ctx, "01JX", false), model.ErrSandboxNotFound)
	assert.ErrorIs(t, mgr.Remove(ctx, "01JX", false), model.ErrSandboxNotFound)
	_, err = mgr.Resolve(ctx, "01JX")
	assert.ErrorIs(t, err, model.ErrSandboxNotFound)
}
