package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

type slowRunner struct{ wait time.Duration }

func (s slowRunner) RunReview(context.Context, controlproto.ReviewRunArgs) ([]byte, error) {
	time.Sleep(s.wait)
	return []byte(`{"schema":"mgit.review-result/v1"}`), nil
}

// A review runs for minutes, far longer than an ordinary control request: the
// client waits for it as long as the run it asked for may take, not for the
// everyday request budget.
func TestClient_ReviewRun_WaitsForTheRunNotJustTheRequestBudget(t *testing.T) {
	client := clientFor(t, func(c *Config) { c.ReviewRunner = slowRunner{wait: 300 * time.Millisecond} })
	client.requestTimeout = 100 * time.Millisecond
	a := runArgs()
	a.MaxSeconds = 5
	got, err := client.ReviewRun(context.Background(), a)
	require.NoError(t, err)
	assert.Contains(t, string(got), "review-result")
}

func TestReviewReadBudget_IsTheRunLimitPlusAGrace(t *testing.T) {
	assert.Equal(t, time.Duration(controlproto.DefaultReviewSeconds)*time.Second+reviewBudgetGrace, reviewReadBudget(0))
	assert.Equal(t, 7*time.Second+reviewBudgetGrace, reviewReadBudget(7))
	assert.Equal(t, time.Duration(controlproto.MaxReviewSeconds)*time.Second+reviewBudgetGrace, reviewReadBudget(controlproto.MaxReviewSeconds))
	assert.GreaterOrEqual(t, reviewBudgetGrace, 30*time.Second)
}

// A completed review's output up to the documented limits crosses the socket
// intact, both ends included: the daemon sends it and the client reads it back
// byte for byte, at the default and at the maximum, split or on one stream.
// Refs: MGIT-333, FEAT-3.347.1.6
func TestClient_ReviewRun_TheDocumentedOutputCrossesTheSocketIntact(t *testing.T) {
	def, maxOut := int(controlproto.DefaultReviewOutputBytes), int(controlproto.MaxReviewOutputBytes)
	for _, c := range []struct {
		name           string
		stdout, stderr int
	}{
		{"1 MiB stdout", 1 << 20, 0},
		{"default split", def - (1 << 20), 1 << 20},
		{"maximum split", maxOut / 2, maxOut / 2},
		{"maximum on stdout", maxOut, 0},
	} {
		want, err := json.Marshal(controlproto.ReviewResult{
			Schema: controlproto.ReviewResultSchema, Lease: strings.Repeat("a", 32),
			Stdout: bytes.Repeat([]byte{'o'}, c.stdout), Stderr: bytes.Repeat([]byte{'e'}, c.stderr),
		})
		require.NoError(t, err)
		client := clientFor(t, func(cfg *Config) { cfg.ReviewRunner = &fakeReviewRunner{res: want} })
		got, err := client.ReviewRun(context.Background(), runArgs())
		require.NoError(t, err, c.name)
		require.True(t, bytes.Equal(want, got), "%s: the result came back changed (%d bytes sent, %d read)", c.name, len(want), len(got))
	}
}
