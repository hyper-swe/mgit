package sandboxd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// verifyingInner is a backend that can say what its guest reads.
type verifyingInner struct {
	*fakeManager
	lastID string
}

func (v *verifyingInner) VerifyGuestView(_ context.Context, id string) (*model.GuestViewReport, error) {
	v.lastID = id
	return &model.GuestViewReport{Checked: 9}, nil
}

// The ceiling hop is a pass-through for the guest-view question: the inner
// backend's answer comes back verbatim, addressed by the same sandbox ID.
// Refs: MGIT-164
func TestCeiling_VerifyGuestView_ForwardsToTheInnerBackend(t *testing.T) {
	inner := &verifyingInner{fakeManager: newFakeManager()}
	mgr := NewCeilingManager(inner, 0, 0, 0)

	got, err := mgr.VerifyGuestView(context.Background(), "01SANDBOX")

	require.NoError(t, err)
	assert.Equal(t, "01SANDBOX", inner.lastID)
	assert.Equal(t, 9, got.Checked)
}

// A backend that cannot answer is reported as unsupported through the hop —
// never as a pass on an empty comparison. Refs: MGIT-164
func TestCeiling_VerifyGuestView_BackendWithoutTheCapability_FailsClosed(t *testing.T) {
	mgr := NewCeilingManager(newFakeManager(), 0, 0, 0)
	_, err := mgr.VerifyGuestView(context.Background(), "01SANDBOX")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrSandboxSyncUnsupported)
}

// failingInner is a backend whose own refusal must reach the caller verbatim.
type failingInner struct{ *fakeManager }

func (failingInner) VerifyGuestView(context.Context, string) (*model.GuestViewReport, error) {
	return nil, errors.New("backend: the guest went away mid-question")
}

// The hop adds nothing to a backend's own refusal: it is forwarded as is.
// Refs: MGIT-164
func TestCeiling_VerifyGuestView_ForwardsTheBackendsRefusalVerbatim(t *testing.T) {
	mgr := NewCeilingManager(failingInner{newFakeManager()}, 0, 0, 0)
	_, err := mgr.VerifyGuestView(context.Background(), "01SANDBOX")
	require.EqualError(t, err, "backend: the guest went away mid-question")
}
