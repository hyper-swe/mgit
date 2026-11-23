package controlproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goodRunArgs() *ReviewRunArgs {
	return &ReviewRunArgs{
		Profile: "reviewer-ro/v1", Image: "reviewer@sha256:" + strings.Repeat("1", 64),
		MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64), BundleDigest: strings.Repeat("d", 64),
		Nonce: "nonce-0123456789abcdef", Lease: strings.Repeat("a", 32),
	}
}

func TestReviewRun_KindIsItsOwnLetter(t *testing.T) {
	assert.Equal(t, byte('Z'), KindReviewRun)
	for _, k := range []byte{KindLaunch, KindExec, KindLand, KindList, KindRemove, KindStatus, KindGrants, KindGrant,
		KindSync, KindPolicySet, KindPolicyShow, KindExport, KindHello, KindEcho, KindSyncVerify, KindReviewPrepare} {
		assert.NotEqual(t, k, KindReviewRun)
	}
}

func TestReviewRun_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	a := goodRunArgs()
	a.MaxSeconds, a.MaxOutputBytes = 30, 1024
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewRun, ReviewRun: a}))
	req, err := ReadRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, a.Lease, req.ReviewRun.Lease)
	assert.Equal(t, 30, req.ReviewRun.MaxSeconds)
	assert.Equal(t, int64(1024), req.ReviewRun.MaxOutputBytes)
}

func TestReviewRun_RefusesBadArguments(t *testing.T) {
	for name, edit := range map[string]func(*ReviewRunArgs){
		"no profile":       func(a *ReviewRunArgs) { a.Profile = "" },
		"no image":         func(a *ReviewRunArgs) { a.Image = "" },
		"no mapping":       func(a *ReviewRunArgs) { a.MappingID = "" },
		"mapping path":     func(a *ReviewRunArgs) { a.MappingID = "../x" },
		"no nonce":         func(a *ReviewRunArgs) { a.Nonce = "" },
		"huge nonce":       func(a *ReviewRunArgs) { a.Nonce = strings.Repeat("n", 300) },
		"no lease":         func(a *ReviewRunArgs) { a.Lease = "" },
		"short lease":      func(a *ReviewRunArgs) { a.Lease = "abc" },
		"lease path":       func(a *ReviewRunArgs) { a.Lease = "../" + strings.Repeat("a", 29) },
		"upper-case lease": func(a *ReviewRunArgs) { a.Lease = strings.Repeat("A", 32) },
		"negative seconds": func(a *ReviewRunArgs) { a.MaxSeconds = -1 },
		"too many seconds": func(a *ReviewRunArgs) { a.MaxSeconds = MaxReviewSeconds + 1 },
		"negative bytes":   func(a *ReviewRunArgs) { a.MaxOutputBytes = -1 },
		"too many bytes":   func(a *ReviewRunArgs) { a.MaxOutputBytes = MaxReviewOutputBytes + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			a := goodRunArgs()
			edit(a)
			var buf bytes.Buffer
			require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewRun, ReviewRun: a}))
			_, err := ReadRequest(&buf)
			assert.Error(t, err)
		})
	}
}

func TestReviewRun_TheLimitsAreTheContract(t *testing.T) {
	assert.Equal(t, 1800, MaxReviewSeconds)
	assert.Equal(t, 600, DefaultReviewSeconds)
	assert.Equal(t, int64(16<<20), MaxReviewOutputBytes)
	assert.Equal(t, int64(4<<20), DefaultReviewOutputBytes)
	a := goodRunArgs()
	a.MaxSeconds, a.MaxOutputBytes = MaxReviewSeconds, MaxReviewOutputBytes
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewRun, ReviewRun: a}))
	_, err := ReadRequest(&buf)
	assert.NoError(t, err, "the maximums themselves are allowed")
}

func TestReviewRun_PayloadMustMatchKind(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindList, ReviewRun: goodRunArgs()}))
	_, err := ReadRequest(&buf)
	assert.Error(t, err)
	buf.Reset()
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewRun}))
	_, err = ReadRequest(&buf)
	assert.Error(t, err)
}

func TestReviewRun_ResultSchemaIsNamed(t *testing.T) {
	assert.Equal(t, "mgit.review-result/v1", ReviewResultSchema)
}
