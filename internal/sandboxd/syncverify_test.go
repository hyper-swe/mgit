package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// verifyingDispatcher is a service that can answer what the guest reads.
type verifyingDispatcher struct {
	*fakeDispatcher
	task   string
	report *model.GuestViewReport
}

func (v *verifyingDispatcher) VerifyGuestView(_ context.Context, taskID string) (*model.GuestViewReport, error) {
	v.task = taskID
	return v.report, nil
}

// The verb round-trips the daemon's answer verbatim: which paths the guest
// reads differently, how many were checked, or why it could not be asked.
// Refs: MGIT-164
func TestClient_VerifyGuestView_RoundTripsTheGuestsAnswer(t *testing.T) {
	svc := &verifyingDispatcher{fakeDispatcher: &fakeDispatcher{},
		report: &model.GuestViewReport{Checked: 3, Stale: []string{"app.go (guest reads the old bytes)"}}}
	client := clientFor(t, func(c *Config) { c.Service = svc })

	got, err := client.VerifyGuestView(context.Background(), "MGIT-1")

	require.NoError(t, err)
	assert.Equal(t, "MGIT-1", svc.task)
	assert.Equal(t, 3, got.Checked)
	assert.Equal(t, []string{"app.go (guest reads the old bytes)"}, got.Stale)
}

// A daemon whose service cannot answer refuses in the operator's words, like
// every other optional verb (MGIT-171) — never with an opcode.
func TestClient_VerifyGuestView_Unwired_RefusesActionably(t *testing.T) {
	client := clientFor(t, func(*Config) {})
	_, err := client.VerifyGuestView(context.Background(), "MGIT-1")
	require.Error(t, err)
	assert.NotRegexp(t, `kind 0x[0-9a-f]+`, err.Error())
	assert.Contains(t, err.Error(), "this daemon")
	assert.Contains(t, err.Error(), "nothing was changed")
}
