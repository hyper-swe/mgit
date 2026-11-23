package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

type runClient struct {
	*fakeSandboxClient
	got controlproto.ReviewRunArgs
	res controlproto.ReviewResult
	err error
}

func (r *runClient) ReviewRun(_ context.Context, a controlproto.ReviewRunArgs) ([]byte, error) {
	r.got = a
	if r.err != nil {
		return nil, r.err
	}
	r.res.Schema = controlproto.ReviewResultSchema
	r.res.Lease = a.Lease
	return json.Marshal(r.res)
}

func execReviewRun(c sandboxClient, args ...string) (stdout, stderr string, err error) {
	cmd := newSandboxCmd(okConnect(c))
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs(append([]string{"review", "run"}, args...))
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func runFlags() []string {
	return []string{
		"--profile", "reviewer-ro/v1", "--image", "reviewer@sha256:" + strings.Repeat("1", 64),
		"--mapping", "pi-v1", "--binding-digest", strings.Repeat("b", 64),
		"--bundle-digest", strings.Repeat("d", 64), "--nonce", "nonce-0123456789abcdef", "--lease", strings.Repeat("a", 32),
	}
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var ee *exitError
	require.True(t, errors.As(err, &ee), "want an exit status, got %v", err)
	return ee.code
}

func TestSandboxReviewRun_DefaultModePassesTheReviewersStreamsAndExitThrough(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, res: controlproto.ReviewResult{ExitCode: 3, Stdout: []byte("out\n"), Stderr: []byte("err\n")}}
	out, errOut, err := execReviewRun(rc, runFlags()...)
	assert.Equal(t, "out\n", out)
	assert.Equal(t, "err\n", errOut)
	assert.Equal(t, 3, exitCodeOf(t, err))
	assert.Equal(t, strings.Repeat("a", 32), rc.got.Lease)
}

func TestSandboxReviewRun_ASuccessfulReviewerExitsZero(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, res: controlproto.ReviewResult{ExitCode: 0, Stdout: []byte("ok")}}
	out, _, err := execReviewRun(rc, runFlags()...)
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
}

func TestSandboxReviewRun_ReservedExitCodesFromTheReviewerAreReportedAsOne(t *testing.T) {
	for _, code := range []int{70, 71} {
		rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, res: controlproto.ReviewResult{ExitCode: code}}
		_, _, err := execReviewRun(rc, runFlags()...)
		assert.Equal(t, 1, exitCodeOf(t, err), "reviewer exit %d must not read as a refusal", code)
	}
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, res: controlproto.ReviewResult{ExitCode: 69}}
	_, _, err := execReviewRun(rc, runFlags()...)
	assert.Equal(t, 69, exitCodeOf(t, err))
}

func TestSandboxReviewRun_JSONModePrintsTheResultAndExitsZeroWhateverTheReviewerExited(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, res: controlproto.ReviewResult{ExitCode: 3, Stdout: []byte("o"), Stderr: []byte("e")}}
	out, errOut, err := execReviewRun(rc, append(runFlags(), "--format", "json")...)
	require.NoError(t, err)
	assert.Empty(t, errOut)
	var got controlproto.ReviewResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 3, got.ExitCode)
	assert.Equal(t, "o", string(got.Stdout))
	assert.Equal(t, "e", string(got.Stderr))
}

func TestSandboxReviewRun_ARefusalBeforeTheReviewerIsAnEnvelopeOnStderrAndExit70(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, err: reviewprep.Refuse(reviewprep.CodeNonceReplayed, "already run")}
	for _, mode := range [][]string{nil, {"--format", "json"}} {
		out, errOut, err := execReviewRun(rc, append(runFlags(), mode...)...)
		assert.Empty(t, out, "a refusal prints nothing on stdout")
		assert.Equal(t, 70, exitCodeOf(t, err))
		assert.Contains(t, errOut, `"schema":"mgit.review-error/v1"`)
		assert.Contains(t, errOut, `"code":"NONCE_REPLAYED"`)
		assert.Contains(t, errOut, `"reviewer_started":false`)
		assert.Equal(t, 1, strings.Count(strings.TrimSpace(errOut), "\n")+1, "exactly one envelope line")
	}
}

func TestSandboxReviewRun_ARefusalAfterTheReviewerStartedExits71(t *testing.T) {
	e := reviewprep.Refuse(reviewprep.CodeRunTimeout, "too slow")
	e.ReviewerStarted = true
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, err: e}
	out, errOut, err := execReviewRun(rc, runFlags()...)
	assert.Empty(t, out)
	assert.Equal(t, 71, exitCodeOf(t, err))
	assert.Contains(t, errOut, `"reviewer_started":true`)
}

func TestSandboxReviewRun_ATransportFailureIsNotAReviewRefusal(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}, err: errors.New("daemon gone")}
	out, errOut, err := execReviewRun(rc, runFlags()...)
	require.Error(t, err)
	assert.Empty(t, out)
	assert.NotContains(t, errOut, "mgit.review-error")
	var ee *exitError
	assert.False(t, errors.As(err, &ee), "an ordinary failure is the usual exit 1")
}

func TestSandboxReviewRun_TheLimitsAreForwardedAndNothingToRunCanBeNamed(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}}
	_, _, _ = execReviewRun(rc, append(runFlags(), "--max-seconds", "30", "--max-output-bytes", "2048")...)
	assert.Equal(t, 30, rc.got.MaxSeconds)
	assert.Equal(t, int64(2048), rc.got.MaxOutputBytes)
	cmd := newSandboxCmd(okConnect(rc))
	run, _, err := cmd.Find([]string{"review", "run"})
	require.NoError(t, err)
	for _, forbidden := range []string{"entrypoint", "argv", "env", "command", "path", "cwd", "exec"} {
		assert.Nil(t, run.Flags().Lookup(forbidden), "the caller must not be able to name what runs: --%s", forbidden)
	}
	for _, need := range []string{"profile", "image", "mapping", "binding-digest", "bundle-digest", "nonce", "lease"} {
		require.NotNil(t, run.Flags().Lookup(need))
	}
}

func TestSandboxReviewRun_OnlyJSONOrNoFormatExists(t *testing.T) {
	rc := &runClient{fakeSandboxClient: &fakeSandboxClient{}}
	_, _, err := execReviewRun(rc, append(runFlags(), "--format", "text")...)
	require.Error(t, err)
}
