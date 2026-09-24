package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Measured on the released v0.6.8: `sandbox status` read "created" before the
// first use, the first boot failed, and status afterwards was byte-identical.
// The only record of the failed boot was daemon.log, so nobody asking what
// state the sandbox is in could tell never-tried from tried-and-failed, which
// is the question status exists to answer. The registration now carries the
// last failed boot, when it happened and its cause, until a boot succeeds.
// Refs: MGIT-231
func TestSandboxService_EnsureRunning_AFailedBootIsRecordedUntilOneSucceeds(t *testing.T) {
	ctx := context.Background()
	mgr := &fakeSandboxManager{launchErr: errors.New(`kvm launch: create vm: firecracker config invalid: failed to stat kernel image path, ""`)}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(ctx, regOpts("MGIT-231", "/work/a"))
	require.NoError(t, err)

	fresh, err := svc.Status(ctx, "MGIT-231")
	require.NoError(t, err)
	assert.Nil(t, fresh.LastBootFailure, "never tried reads as never tried")

	_, err = svc.EnsureRunning(ctx, "MGIT-231")
	require.Error(t, err)
	failed, err := svc.Status(ctx, "MGIT-231")
	require.NoError(t, err)
	require.NotNil(t, failed.LastBootFailure, "tried and failed must not read like never tried")
	assert.Equal(t, time.Unix(0, 0).UTC(), failed.LastBootFailure.At, "when, from the service clock")
	assert.Contains(t, failed.LastBootFailure.Cause, "failed to stat kernel image path", "and why")
	assert.Equal(t, model.StateCreated, failed.State, "the registration stays retryable")
	listed, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.NotNil(t, listed[0].LastBootFailure, "list reports it too")

	mgr.launchErr = nil
	_, err = svc.EnsureRunning(ctx, "MGIT-231")
	require.NoError(t, err)
	booted, err := svc.Status(ctx, "MGIT-231")
	require.NoError(t, err)
	assert.Nil(t, booted.LastBootFailure, "a later successful boot clears the record")
}
