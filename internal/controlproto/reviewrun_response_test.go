package controlproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reviewResponse is a review-run answer carrying a result with the given
// stdout and stderr sizes, encoded the way the run service encodes it.
func reviewResponse(t *testing.T, stdout, stderr int) *Response {
	t.Helper()
	raw, err := json.Marshal(ReviewResult{
		Schema: ReviewResultSchema, Lease: strings.Repeat("a", 32), ExitCode: 0,
		Stdout: bytes.Repeat([]byte{0xff}, stdout), Stderr: bytes.Repeat([]byte{0xfe}, stderr), ElapsedMS: 1 << 40,
	})
	require.NoError(t, err)
	return &Response{ReviewRun: &ReviewRunResult{Result: raw}}
}

// A completed review whose output is within the documented limits must be
// sendable: the default (4 MiB) and the maximum (16 MiB), split between stdout
// and stderr or all on one stream, and the 1 MiB case that first failed.
// Refs: MGIT-333, FEAT-3.347.1.6
func TestWriteResponse_AReviewResultUpToTheDocumentedMaximumIsSent(t *testing.T) {
	def, maxOut := int(DefaultReviewOutputBytes), int(MaxReviewOutputBytes)
	for _, c := range []struct {
		name           string
		stdout, stderr int
	}{
		{"1 MiB stdout", 1 << 20, 0},
		{"default split", def - (1 << 20), 1 << 20},
		{"default on stdout", def, 0},
		{"maximum split", maxOut / 2, maxOut / 2},
		{"maximum on stdout", maxOut, 0},
		{"maximum on stderr", 0, maxOut},
		{"maximum split unevenly", maxOut - 1, 1},
	} {
		var buf bytes.Buffer
		require.NoError(t, WriteResponse(&buf, reviewResponse(t, c.stdout, c.stderr)), c.name)
		assert.Greater(t, buf.Len(), 0, c.name)
	}
}

// Only a review-run answer gets the larger ceiling: every other verb's answer,
// and a review-run answer that also carries any other field, keeps the 1 MiB
// control-response cap.
func TestWriteResponse_OtherAnswersKeepTheOneMebibyteCap(t *testing.T) {
	big := strings.Repeat("x", MaxResponseBytes)
	err := WriteResponse(&bytes.Buffer{}, &Response{Error: big})
	assert.True(t, errors.Is(err, ErrResponseTooLarge), "a non-review answer over 1 MiB was sent: %v", err)

	mixed := reviewResponse(t, 2<<20, 0)
	mixed.Error = "x"
	err = WriteResponse(&bytes.Buffer{}, mixed)
	assert.True(t, errors.Is(err, ErrResponseTooLarge), "a review answer carrying another field got the review ceiling: %v", err)

	refusal := &Response{ReviewRun: &ReviewRunResult{Refusal: json.RawMessage(`"` + big + `"`)}}
	err = WriteResponse(&bytes.Buffer{}, refusal)
	assert.True(t, errors.Is(err, ErrResponseTooLarge), "a refusal envelope got the review ceiling: %v", err)
}

// The everyday reader still refuses a frame over 1 MiB before allocating it.
func TestReadResponse_StillRefusesAFrameOverOneMebibyte(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(MaxResponseBytes+1))
	_, err := ReadResponse(bytes.NewReader(hdr[:]))
	require.Error(t, err)
}
