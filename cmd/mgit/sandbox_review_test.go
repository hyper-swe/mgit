package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

type reviewClient struct {
	*fakeSandboxClient
	got controlproto.ReviewPrepareArgs
	att []byte
	err error
}

func (r *reviewClient) ReviewPrepare(_ context.Context, a controlproto.ReviewPrepareArgs) ([]byte, error) {
	r.got = a
	return r.att, r.err
}

func runReview(c sandboxClient, args ...string) (stdout, stderr string, err error) {
	cmd := newSandboxCmd(okConnect(c))
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs(append([]string{"review", "prepare"}, args...))
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func reviewFlags(dir string) []string {
	return []string{
		"--profile", "reviewer-ro/v1", "--image", "reviewer@sha256:" + strings.Repeat("1", 64),
		"--mapping", "pi-v1", "--binding-digest", strings.Repeat("b", 64),
		"--bundle", dir, "--bundle-digest", strings.Repeat("d", 64), "--nonce", "nonce-0123456789abcdef", "--format", "json",
	}
}

func TestSandboxReviewPrepare_PrintsOnlyTheAttestation(t *testing.T) {
	rc := &reviewClient{fakeSandboxClient: &fakeSandboxClient{}, att: []byte(`{"schema":"mgit.review-attest/v1"}`)}
	dir := t.TempDir()

	out, errOut, err := runReview(rc, reviewFlags(dir)...)

	require.NoError(t, err)
	assert.JSONEq(t, `{"schema":"mgit.review-attest/v1"}`, out)
	assert.Empty(t, errOut)
	assert.Equal(t, "pi-v1", rc.got.MappingID)
	assert.Equal(t, canonicalPath(dir), rc.got.BundleDir, "the bundle path is made absolute before it reaches the daemon")
}

func TestSandboxReviewPrepare_RefusalIsAnEnvelopeOnStderrAndNonzero(t *testing.T) {
	rc := &reviewClient{fakeSandboxClient: &fakeSandboxClient{},
		err: reviewprep.Refuse(reviewprep.CodeImageMismatch, "image digest does not match the mapping")}

	out, errOut, err := runReview(rc, reviewFlags(t.TempDir())...)

	require.Error(t, err)
	assert.Empty(t, out, "a refusal prints nothing on stdout")
	assert.Contains(t, errOut, `"schema":"mgit.review-error/v1"`)
	assert.Contains(t, errOut, `"code":"IMAGE_MISMATCH"`)
	assert.Contains(t, errOut, `"reviewer_started":false`)
}

func TestSandboxReviewPrepare_OnlyTheJSONFormatExists(t *testing.T) {
	rc := &reviewClient{fakeSandboxClient: &fakeSandboxClient{}, att: []byte(`{}`)}
	flags := append(reviewFlags(t.TempDir()), "--format", "text")
	_, _, err := runReview(rc, flags...)
	require.Error(t, err)
}

func TestSandboxReviewPrepare_AnUnreachableDaemonIsNotARefusalEnvelope(t *testing.T) {
	rc := &reviewClient{fakeSandboxClient: &fakeSandboxClient{}, err: errors.New("daemon down")}
	_, errOut, err := runReview(rc, reviewFlags(filepath.Join(t.TempDir(), "x"))...)
	require.Error(t, err)
	assert.NotContains(t, errOut, "mgit.review-error/v1")
}

func TestSandboxReviewPrepare_AClientWithoutTheVerbSaysSo(t *testing.T) {
	_, _, err := runReview(&fakeSandboxClient{}, reviewFlags(t.TempDir())...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot")
}

func TestSandboxReviewPrepare_RequiresEveryFlag(t *testing.T) {
	_, _, err := runReview(&reviewClient{fakeSandboxClient: &fakeSandboxClient{}}, "--profile", "reviewer-ro/v1")
	require.Error(t, err)
}
