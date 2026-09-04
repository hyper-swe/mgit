package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The probe asks the daemon — which owns the delivered manifest — rather than
// reading the daemon's state directory itself. Refs: MGIT-164, MGIT-154
func TestProbeGuestDelivery_AsksTheDaemonForTheBoundTask(t *testing.T) {
	c := &fakeSandboxClient{verifyReport: &model.GuestViewReport{Checked: 7}}
	report, err := probeGuestDelivery(context.Background(), okConnect(c), "MGIT-76")
	require.NoError(t, err)
	assert.Equal(t, 7, report.Checked)
	assert.Equal(t, "MGIT-76", c.verifyTask)
}

func TestProbeGuestDelivery_NoTaskOrNoDaemon_IsAnError(t *testing.T) {
	_, err := probeGuestDelivery(context.Background(), nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sandbox")

	failing := func(context.Context) (sandboxClient, error) { return nil, errors.New("dial: refused") }
	_, err = probeGuestDelivery(context.Background(), failing, "MGIT-76")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sandbox daemon")
}
