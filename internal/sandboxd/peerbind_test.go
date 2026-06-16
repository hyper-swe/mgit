package sandboxd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// binderWithLog returns a PeerBinder writing audit events to buf.
func binderWithLog() (*PeerBinder, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return NewPeerBinder(slog.New(slog.NewTextHandler(buf, nil))), buf
}

// TestVsock_CIDMismatch_Rejected verifies a connection whose source peer
// identity differs from the sandbox's binding is rejected. Refs: SEC-10, FR-17.27
func TestVsock_CIDMismatch_Rejected(t *testing.T) {
	b, _ := binderWithLog()
	b.Bind("01JXSBA000000000000000000", "cid:3")

	require.NoError(t, b.Authorize("01JXSBA000000000000000000", "cid:3"), "the bound peer is authorized")
	err := b.Authorize("01JXSBA000000000000000000", "cid:4")
	assert.ErrorIs(t, err, model.ErrPeerBindingMismatch, "a different CID is rejected")
}

// TestVsock_CrossSandboxChannel_Unreachable verifies one guest cannot
// address another sandbox's channel. Refs: SEC-10, FR-17.27
func TestVsock_CrossSandboxChannel_Unreachable(t *testing.T) {
	b, _ := binderWithLog()
	b.Bind("01JXSBA000000000000000000", "cid:3") // guest A
	b.Bind("01JXSBB000000000000000000", "cid:5") // guest B

	// Guest B (cid:5) tries to reach guest A's land/attestation channel.
	err := b.Authorize("01JXSBA000000000000000000", "cid:5")
	assert.ErrorIs(t, err, model.ErrPeerBindingMismatch,
		"guest B must not reach guest A's channel")
	// Each guest can still reach its own.
	assert.NoError(t, b.Authorize("01JXSBB000000000000000000", "cid:5"))
}

// TestVsock_CIDBindingAudited verifies every rejection is written to the
// audit log. Refs: SEC-10, FR-17.27, FR-17.18
func TestVsock_CIDBindingAudited(t *testing.T) {
	b, buf := binderWithLog()
	b.Bind("01JXSBA000000000000000000", "cid:3")
	_ = b.Authorize("01JXSBA000000000000000000", "cid:9")
	assert.Contains(t, buf.String(), "peer_rejected", "the rejection is audited")
	assert.Contains(t, buf.String(), "01JXSBA000000000000000000", "the addressed sandbox is named")
}

// TestVsock_Teardown_InvalidatesBinding verifies that after teardown a
// sandbox's channel can no longer be addressed, and a recycled CID
// assigned to a successor does NOT inherit the old binding. Refs: FR-17.27
func TestVsock_Teardown_InvalidatesBinding(t *testing.T) {
	b, _ := binderWithLog()
	b.Bind("01JXSBOLD000000000000000", "cid:3")
	require.NoError(t, b.Authorize("01JXSBOLD000000000000000", "cid:3"))

	b.Invalidate("01JXSBOLD000000000000000")
	assert.ErrorIs(t, b.Authorize("01JXSBOLD000000000000000", "cid:3"),
		model.ErrPeerBindingMismatch, "a torn-down sandbox's channel is unreachable")

	// The hypervisor recycles cid:3 for a new sandbox.
	b.Bind("01JXSBNEW000000000000000", "cid:3")
	assert.ErrorIs(t, b.Authorize("01JXSBOLD000000000000000", "cid:3"),
		model.ErrPeerBindingMismatch, "the old sandbox does not inherit the recycled CID")
	assert.NoError(t, b.Authorize("01JXSBNEW000000000000000", "cid:3"),
		"the successor owns the recycled CID")
}

// TestVsock_UnboundOrEmpty_Rejected covers the fail-closed branches:
// addressing a sandbox with no binding, or an empty source peer.
func TestVsock_UnboundOrEmpty_Rejected(t *testing.T) {
	b, _ := binderWithLog()
	assert.ErrorIs(t, b.Authorize("01JXSBNONE00000000000000", "cid:3"),
		model.ErrPeerBindingMismatch, "no binding for the sandbox → reject")
	b.Bind("01JXSBA000000000000000000", "cid:3")
	assert.ErrorIs(t, b.Authorize("01JXSBA000000000000000000", ""),
		model.ErrPeerBindingMismatch, "an unverifiable (empty) peer → reject")
}

// TestVsock_Rebind_ReplacesBinding verifies a relaunch rebinds cleanly.
func TestVsock_Rebind_ReplacesBinding(t *testing.T) {
	b, _ := binderWithLog()
	b.Bind("01JXSBA000000000000000000", "cid:3")
	b.Bind("01JXSBA000000000000000000", "cid:7") // relaunch with a new CID
	assert.ErrorIs(t, b.Authorize("01JXSBA000000000000000000", "cid:3"), model.ErrPeerBindingMismatch)
	assert.NoError(t, b.Authorize("01JXSBA000000000000000000", "cid:7"))
}

// TestVsock_NoAuditOnSuccess verifies authorized connections are not
// logged as rejections (no false positives in the audit trail).
func TestVsock_NoAuditOnSuccess(t *testing.T) {
	b, buf := binderWithLog()
	b.Bind("01JXSBA000000000000000000", "cid:3")
	require.NoError(t, b.Authorize("01JXSBA000000000000000000", "cid:3"))
	assert.False(t, strings.Contains(buf.String(), "peer_rejected"))
}
