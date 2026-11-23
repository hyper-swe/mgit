package sandboxd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

type fakeReviewPreparer struct {
	got controlproto.ReviewPrepareArgs
	att []byte
	err error
}

func (f *fakeReviewPreparer) PrepareReview(_ context.Context, a controlproto.ReviewPrepareArgs) ([]byte, error) {
	f.got = a
	return f.att, f.err
}

func reviewArgs() controlproto.ReviewPrepareArgs {
	return controlproto.ReviewPrepareArgs{Profile: "reviewer-ro/v1", Image: "r@sha256:" + string(make([]byte, 0)),
		MappingID: "pi-v1", BindingDigest: "b", BundleDir: "/tmp/e", BundleDigest: "d", Nonce: "nonce-0123456789abcdef"}
}

func TestClient_ReviewPrepare_ReturnsTheAttestationVerbatim(t *testing.T) {
	svc := &fakeReviewPreparer{att: []byte(`{"schema":"mgit.review-attest/v1"}`)}
	client := clientFor(t, func(c *Config) { c.ReviewPreparer = svc })

	got, err := client.ReviewPrepare(context.Background(), reviewArgs())

	require.NoError(t, err)
	assert.JSONEq(t, `{"schema":"mgit.review-attest/v1"}`, string(got))
	assert.Equal(t, "pi-v1", svc.got.MappingID)
}

func TestClient_ReviewPrepare_RefusalComesBackTyped(t *testing.T) {
	svc := &fakeReviewPreparer{err: &reviewprep.Error{Schema: reviewprep.ErrorSchema, Code: "IMAGE_MISMATCH", Phase: "prepare", Detail: "no"}}
	client := clientFor(t, func(c *Config) { c.ReviewPreparer = svc })

	_, err := client.ReviewPrepare(context.Background(), reviewArgs())

	var e *reviewprep.Error
	require.True(t, errors.As(err, &e), "got %v", err)
	assert.Equal(t, "IMAGE_MISMATCH", e.Code)
	assert.False(t, e.ReviewerStarted)
}

func TestClient_ReviewPrepare_UnwiredRefusesActionably(t *testing.T) {
	client := clientFor(t, func(*Config) {})
	_, err := client.ReviewPrepare(context.Background(), reviewArgs())
	require.Error(t, err)
	assert.NotRegexp(t, `kind 0x[0-9a-f]+`, err.Error())
	assert.Contains(t, err.Error(), "this daemon")
	assert.Contains(t, err.Error(), "nothing was changed")
}
