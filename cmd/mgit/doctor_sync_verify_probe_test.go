package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The probe asks the guest, through its exec channel, for exactly the two
// things a sync's guest-side confirmation relies on: a sha256sum to read
// the delivered digest with, and a writable drop_caches to invalidate the
// guest's stale view with. Refs: MGIT-192
func TestProbeGuestSyncVerify_AsksForTheToolsASyncConfirmsWith(t *testing.T) {
	c := &fakeSandboxClient{execStdout: "sha256sum\ndrop_caches\n"}
	out, err := probeGuestSyncVerify(context.Background(), okConnect(c), "MGIT-76")
	require.NoError(t, err)
	assert.Equal(t, "sha256sum\ndrop_caches\n", out)
	script := strings.Join(c.execReq.Command, " ")
	assert.Contains(t, script, "sha256sum")
	assert.Contains(t, script, "/proc/sys/vm/drop_caches")
	assert.Equal(t, "MGIT-76", c.execTask)
}

func TestProbeGuestSyncVerify_NoTaskOrNoDaemon_IsAnError(t *testing.T) {
	_, err := probeGuestSyncVerify(context.Background(), nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sandbox")

	failing := func(context.Context) (sandboxClient, error) { return nil, errors.New("dial: refused") }
	_, err = probeGuestSyncVerify(context.Background(), failing, "MGIT-76")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sandbox daemon")
}
