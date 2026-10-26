package service

import (
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The documented stale-base remedy must release the existing task/worktree
// binding before registering the replacement. No guest is booted here.
func TestStaleBase_RemoveThenRegisterReplacement(t *testing.T) {
	svc := newSvc(t, &fakeSandboxManager{}, &fakeEventAppender{})
	opts := regOpts("MGIT-286", "/work/stale-base")
	opts.ImageRef = "base@sha256:" + strings.Repeat("a", 64)
	_, err := svc.Register(t.Context(), opts)
	require.NoError(t, err)
	opts.ImageRef = "base@sha256:" + strings.Repeat("b", 64)
	_, err = svc.Register(t.Context(), opts)
	require.ErrorIs(t, err, model.ErrTaskAlreadyBound)
	require.NoError(t, svc.Remove(t.Context(), opts.TaskID, false))
	replacement, err := svc.Register(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, opts.TaskID, replacement.TaskID)
	assert.Equal(t, opts.WorktreePath, replacement.WorktreePath)
}
