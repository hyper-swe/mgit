package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"net/netip"
)

// denial builds a valid host-observed egress denial for the tests.
func denial(sandboxID, taskID, ip string, port int) model.ObservedDenial {
	return model.ObservedDenial{
		SandboxID: sandboxID, TaskID: taskID,
		DestIP: netip.MustParseAddr(ip), DestPort: port,
		Rule: "raw-ip not allowlisted",
	}
}

// TestCap_RecordDenial_PendingThenApprove is the deny->prompt->Grant flow: a
// host-observed denial becomes a pending request the operator can list and
// approve, and approval turns it into a live, audited grant that widens the
// sandbox's allowlist. Refs: FR-17.12, SEC-05
func TestCap_RecordDenial_PendingThenApprove(t *testing.T) {
	events := &recordingEventAppender{}
	granter := newRecordingGranter()
	svc, err := NewCapabilityService(events, granter, fixedClock())
	require.NoError(t, err)
	ctx := context.Background()

	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443))

	pending := svc.PendingRequests("sbx-1")
	require.Len(t, pending, 1)
	assert.Equal(t, "203.0.113.7", pending[0].ObservedDestIP)
	assert.Equal(t, 443, pending[0].ObservedDestPort)
	key := pending[0].Key()
	assert.Equal(t, "203.0.113.7:443", key)

	grant, err := svc.Approve(ctx, "sbx-1", key)
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.7", grant.ObservedDestIP)
	// The live allowlist was widened with exactly the host-observed entry.
	assert.Equal(t, []string{"203.0.113.7:443"}, granter.added["sbx-1"])
	// Approval clears the pending request and records an append-only audit event.
	assert.Empty(t, svc.PendingRequests("sbx-1"))
	require.Len(t, events.events, 1)
	assert.Equal(t, model.EventPolicyGranted, events.events[0].EventType)
}

// TestCap_RecordDenial_Deduplicates: the same destination is recorded once
// (one prompt per capability), and an already-granted destination is not
// re-recorded. Refs: FR-17.12
func TestCap_RecordDenial_Deduplicates(t *testing.T) {
	svc, err := NewCapabilityService(&recordingEventAppender{}, newRecordingGranter(), fixedClock())
	require.NoError(t, err)

	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443))
	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443)) // duplicate
	assert.Len(t, svc.PendingRequests("sbx-1"), 1, "duplicate denial is recorded once")

	// A different destination is a separate pending request.
	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.8", 80))
	assert.Len(t, svc.PendingRequests("sbx-1"), 2)

	// Approving one, then re-denying the same dest, does not re-record it.
	_, err = svc.Approve(context.Background(), "sbx-1", "203.0.113.7:443")
	require.NoError(t, err)
	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443))
	for _, p := range svc.PendingRequests("sbx-1") {
		assert.NotEqual(t, "203.0.113.7:443", p.Key(), "an already-granted dest is not re-recorded")
	}
}

// TestCap_RecordDenial_InvalidSkipped: a denial without a concrete host
// destination (zero IP) cannot form a grantable request and is skipped.
func TestCap_RecordDenial_InvalidSkipped(t *testing.T) {
	svc, err := NewCapabilityService(&recordingEventAppender{}, newRecordingGranter(), fixedClock())
	require.NoError(t, err)
	svc.RecordDenial(model.ObservedDenial{SandboxID: "sbx-1", TaskID: "MGIT-1", DestPort: 443})
	assert.Empty(t, svc.PendingRequests("sbx-1"), "a denial with no host IP is not grantable")
}

// TestCap_Approve_UnknownKey_Error: approving a non-pending key is rejected.
func TestCap_Approve_UnknownKey_Error(t *testing.T) {
	svc, err := NewCapabilityService(&recordingEventAppender{}, newRecordingGranter(), fixedClock())
	require.NoError(t, err)
	_, err = svc.Approve(context.Background(), "sbx-1", "203.0.113.7:443")
	assert.ErrorIs(t, err, model.ErrCapabilityGrantNotFound)
}

// TestCap_Revoke_ClearsPending: teardown drops pending requests too — they are
// scoped to the sandbox lifetime. Refs: FR-17.12
func TestCap_Revoke_ClearsPending(t *testing.T) {
	svc, err := NewCapabilityService(&recordingEventAppender{}, newRecordingGranter(), fixedClock())
	require.NoError(t, err)
	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443))
	require.Len(t, svc.PendingRequests("sbx-1"), 1)
	svc.Revoke("sbx-1")
	assert.Empty(t, svc.PendingRequests("sbx-1"))
}

// TestCap_Approve_WidenFailure_LeavesPending: a transient grant failure leaves
// the request approvable again (pending not dropped). Refs: FR-17.12
func TestCap_Approve_WidenFailure_LeavesPending(t *testing.T) {
	granter := newRecordingGranter()
	granter.addErr = errors.New("egress engine down")
	svc, err := NewCapabilityService(&recordingEventAppender{}, granter, fixedClock())
	require.NoError(t, err)
	svc.RecordDenial(denial("sbx-1", "MGIT-1", "203.0.113.7", 443))
	_, err = svc.Approve(context.Background(), "sbx-1", "203.0.113.7:443")
	assert.Error(t, err)
	assert.Len(t, svc.PendingRequests("sbx-1"), 1, "a failed grant leaves the request approvable")
}
