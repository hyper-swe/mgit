package sandboxd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

type fakeReviewRunner struct {
	got controlproto.ReviewRunArgs
	res []byte
	err error
}

func (f *fakeReviewRunner) RunReview(_ context.Context, a controlproto.ReviewRunArgs) ([]byte, error) {
	f.got = a
	return f.res, f.err
}

func runArgs() controlproto.ReviewRunArgs {
	return controlproto.ReviewRunArgs{Profile: "reviewer-ro/v1", Image: "r@sha256:" + strings.Repeat("1", 64),
		MappingID: "pi-v1", BindingDigest: "b", BundleDigest: "d", Nonce: "nonce-0123456789abcdef", Lease: strings.Repeat("a", 32)}
}

func TestClient_ReviewRun_ReturnsTheResultVerbatim(t *testing.T) {
	svc := &fakeReviewRunner{res: []byte(`{"schema":"mgit.review-result/v1","exit_code":0}`)}
	client := clientFor(t, func(c *Config) { c.ReviewRunner = svc })

	got, err := client.ReviewRun(context.Background(), runArgs())

	require.NoError(t, err)
	assert.JSONEq(t, `{"schema":"mgit.review-result/v1","exit_code":0}`, string(got))
	assert.Equal(t, strings.Repeat("a", 32), svc.got.Lease)
}

func TestClient_ReviewRun_RefusalComesBackTypedWithWhetherTheReviewerStarted(t *testing.T) {
	for name, want := range map[string]bool{"before": false, "after": true} {
		svc := &fakeReviewRunner{err: &reviewprep.Error{Schema: reviewprep.ErrorSchema, Code: reviewprep.CodeRunTimeout,
			Phase: "run", Detail: "no", ReviewerStarted: want}}
		client := clientFor(t, func(c *Config) { c.ReviewRunner = svc })

		_, err := client.ReviewRun(context.Background(), runArgs())

		var e *reviewprep.Error
		require.True(t, errors.As(err, &e), "%s: got %v", name, err)
		assert.Equal(t, reviewprep.CodeRunTimeout, e.Code)
		assert.Equal(t, want, e.ReviewerStarted, name)
	}
}

func TestClient_ReviewRun_UnwiredRefusesActionably(t *testing.T) {
	client := clientFor(t, func(*Config) {})
	_, err := client.ReviewRun(context.Background(), runArgs())
	require.Error(t, err)
	assert.NotRegexp(t, `kind 0x[0-9a-f]+`, err.Error())
	assert.Contains(t, err.Error(), "this daemon")
	assert.Contains(t, err.Error(), "nothing was started")
}

func TestClient_ReviewRun_ATransportFailureIsNotAReviewRefusal(t *testing.T) {
	svc := &fakeReviewRunner{err: errors.New("disk on fire")}
	client := clientFor(t, func(c *Config) { c.ReviewRunner = svc })
	_, err := client.ReviewRun(context.Background(), runArgs())
	require.Error(t, err)
	var e *reviewprep.Error
	assert.False(t, errors.As(err, &e))
}
