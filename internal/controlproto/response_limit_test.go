package controlproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// responseOfSize returns a Response whose encoded form is exactly want bytes,
// built by binary search on a padding field rather than by arithmetic.
//
// Computing the padding would mean re-deriving JSON's escaping and envelope in
// the test — the same reasoning the subject does, so the two would agree while
// both being wrong. Measuring the encode is the only way the boundary case is
// actually AT the boundary. Refs: MGIT-160
func responseOfSize(t *testing.T, want int) *Response {
	t.Helper()
	size := func(pad int) int {
		b, err := json.Marshal(&Response{Error: strings.Repeat("x", pad)})
		require.NoError(t, err)
		return len(b)
	}
	require.Less(t, size(0), want, "the envelope alone already exceeds the target")

	lo, hi := 0, want
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if size(mid) <= want {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	resp := &Response{Error: strings.Repeat("x", lo)}
	require.Equal(t, want, size(lo),
		"could not hit the target size exactly; the search or the encoding changed")
	return resp
}

// THE BOUNDARY ITSELF, measured rather than estimated.
//
// MGIT-160 was a response that could not be sent producing a bare EOF. The cap
// is where "sendable" stops, so it is the one size worth pinning exactly — and
// the case list is derived from MaxResponseBytes, the constant the subject
// itself reads, not from a number a test author guessed. Refs: MGIT-160
func TestWriteResponse_TheSizeBoundary(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		wantSent bool
	}{
		{name: "one_byte_under_the_cap_is_sent", size: MaxResponseBytes - 1, wantSent: true},
		{name: "exactly_at_the_cap_is_sent", size: MaxResponseBytes, wantSent: true},
		{name: "one_byte_over_the_cap_is_refused", size: MaxResponseBytes + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := responseOfSize(t, tt.size)
			var buf bytes.Buffer

			err := WriteResponse(&buf, resp)

			if tt.wantSent {
				require.NoError(t, err, "a response within the cap must be sent")
				assert.NotZero(t, buf.Len(), "and must actually reach the wire")
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrResponseTooLarge),
				"the refusal must be the SENTINEL: the daemon has to tell an unsendable "+
					"answer from a dead connection, and matching on a message breaks the "+
					"moment the message improves")
			assert.Zero(t, buf.Len(),
				"an over-size response must write NOTHING — a partial frame is what the "+
					"reader saw as EOF")
		})
	}
}

// A response at exactly the cap must survive the ROUND TRIP, not merely be
// written: ReadResponse enforces the same ceiling before allocating, and a
// reader that rejected what the writer accepted would make the boundary
// unusable from one side. Refs: MGIT-160
func TestResponse_ExactlyAtTheCap_RoundTrips(t *testing.T) {
	resp := responseOfSize(t, MaxResponseBytes)
	var buf bytes.Buffer
	require.NoError(t, WriteResponse(&buf, resp))

	got, err := ReadResponse(&buf)

	require.NoError(t, err, "the writer and the reader must agree on where the cap is")
	assert.Equal(t, resp.Error, got.Error)
}

// The refusal names the two integers a caller acts on and points at what to
// narrow. It is built from the SIZES, never from the payload — a refusal
// assembled out of the thing that was too big would fail the same way.
// Refs: MGIT-160
func TestWriteResponse_TheRefusalNamesTheSizeTheCapAndARecourse(t *testing.T) {
	over := MaxResponseBytes + 1
	err := WriteResponse(&bytes.Buffer{}, responseOfSize(t, over))

	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "1048576", "the cap it hit")
	assert.Contains(t, msg, "Nothing was applied",
		"a caller must know there is nothing to undo")
	assert.Contains(t, msg, "node_modules",
		"and must be pointed at the thing that is actually making the answer large")
	assert.NotContains(t, strings.ToLower(msg), "eof",
		"the whole point is that this is no longer an EOF")
}

// A BOUNDED sync report is sendable — the composition MGIT-160 relies on.
// model.Bound caps the counts and controlproto enforces the bytes; nothing
// asserted that the first is enough for the second at a realistic tree size.
// Refs: MGIT-160
func TestWriteResponse_ABoundedSyncReport_IsSendable(t *testing.T) {
	paths := make([]string, 40_000)
	for i := range paths {
		paths[i] = "node_modules/@scope/package-" + strings.Repeat("x", 40) + "/dist/index.js"
	}
	full := &model.WorktreeSyncReport{Updated: paths, Deleted: paths}

	// Unbounded, this is the MGIT-160 failure.
	require.ErrorIs(t, WriteResponse(&bytes.Buffer{}, &Response{Synced: full}),
		ErrResponseTooLarge, "the premise: the raw report is what could not be sent")

	bounded := full.Bound(model.SyncReportPathLimit)
	var buf bytes.Buffer

	require.NoError(t, WriteResponse(&buf, &Response{Synced: &bounded}),
		"bounding at the model must make the answer sendable at the transport")

	got, err := ReadResponse(&buf)
	require.NoError(t, err)
	require.NotNil(t, got.Synced)
	assert.Equal(t, 40_000, got.Synced.UpdatedTotal,
		"and the honest total must survive the crossing, or the caller is told 500")
	assert.True(t, got.Synced.Truncated,
		"and the listing must announce itself as partial")
}
