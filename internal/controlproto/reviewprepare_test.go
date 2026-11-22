package controlproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goodReviewArgs() *ReviewPrepareArgs {
	return &ReviewPrepareArgs{
		Profile: "reviewer-ro/v1", Image: "reviewer@sha256:" + strings.Repeat("1", 64),
		MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDir: "/tmp/evidence", BundleDigest: strings.Repeat("d", 64),
		Nonce: "nonce-0123456789abcdef",
	}
}

func TestReviewPrepare_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewPrepare, ReviewPrepare: goodReviewArgs()}))
	req, err := ReadRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, "pi-v1", req.ReviewPrepare.MappingID)
}

func TestReviewPrepare_RefusesBadArguments(t *testing.T) {
	for name, edit := range map[string]func(*ReviewPrepareArgs){
		"no profile":      func(a *ReviewPrepareArgs) { a.Profile = "" },
		"no image":        func(a *ReviewPrepareArgs) { a.Image = "" },
		"no mapping":      func(a *ReviewPrepareArgs) { a.MappingID = "" },
		"mapping path":    func(a *ReviewPrepareArgs) { a.MappingID = "../x" },
		"relative bundle": func(a *ReviewPrepareArgs) { a.BundleDir = "evidence" },
		"nul in bundle":   func(a *ReviewPrepareArgs) { a.BundleDir = "/tmp/a\x00b" },
		"no nonce":        func(a *ReviewPrepareArgs) { a.Nonce = "" },
		"huge nonce":      func(a *ReviewPrepareArgs) { a.Nonce = strings.Repeat("n", 300) },
	} {
		t.Run(name, func(t *testing.T) {
			a := goodReviewArgs()
			edit(a)
			var buf bytes.Buffer
			require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewPrepare, ReviewPrepare: a}))
			_, err := ReadRequest(&buf)
			assert.Error(t, err)
		})
	}
}

func TestReviewPrepare_PayloadMustMatchKind(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindList, ReviewPrepare: goodReviewArgs()}))
	_, err := ReadRequest(&buf)
	assert.Error(t, err)
	buf.Reset()
	require.NoError(t, WriteRequest(&buf, &Request{Kind: KindReviewPrepare}))
	_, err = ReadRequest(&buf)
	assert.Error(t, err)
}
