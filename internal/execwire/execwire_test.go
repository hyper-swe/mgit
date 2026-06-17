package execwire

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestRequest_RoundTrip verifies a request written by WriteRequest reads
// back identically through ReadRequest.
func TestRequest_RoundTrip(t *testing.T) {
	req := model.ExecRequest{Command: []string{"sh", "-c", "echo hi"}, Dir: "/work", Env: []string{"K=v"}}
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, req))

	got, err := ReadRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, req.Command, got.Command)
	assert.Equal(t, req.Dir, got.Dir)
	assert.Equal(t, req.Env, got.Env)
}

// TestReadRequest_OversizeRejected verifies the untrusted-peer ceiling is
// enforced before allocating.
func TestReadRequest_OversizeRejected(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxRequestBytes+1)
	_, err := ReadRequest(bytes.NewReader(hdr[:]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap")
}

// TestReadRequest_Truncated verifies short reads fail closed.
func TestReadRequest_Truncated(t *testing.T) {
	t.Run("short_header", func(t *testing.T) {
		_, err := ReadRequest(bytes.NewReader([]byte{0x00, 0x01}))
		assert.Error(t, err)
	})
	t.Run("short_body", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 100) // claims 100 bytes, supplies none
		_, err := ReadRequest(bytes.NewReader(hdr[:]))
		assert.Error(t, err)
	})
	t.Run("malformed_json", func(t *testing.T) {
		var buf bytes.Buffer
		payload := []byte("{not json")
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
		buf.Write(hdr[:])
		buf.Write(payload)
		_, err := ReadRequest(&buf)
		assert.Error(t, err)
	})
}

// TestFrame_RoundTrip verifies a written frame reads back with the same
// kind and payload, including an empty payload.
func TestFrame_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    byte
		payload []byte
	}{
		{"stdout", FrameStdout, []byte("hello")},
		{"stderr", FrameStderr, []byte("oops")},
		{"empty", FrameResult, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteFrame(&buf, tc.kind, tc.payload))
			kind, payload, err := ReadFrame(&buf)
			require.NoError(t, err)
			assert.Equal(t, tc.kind, kind)
			assert.Equal(t, len(tc.payload), len(payload))
		})
	}
}

// TestReadFrame_EOF verifies a clean end-of-stream surfaces as io.EOF so
// a caller can detect a closed connection without a result frame.
func TestReadFrame_EOF(t *testing.T) {
	_, _, err := ReadFrame(bytes.NewReader(nil))
	assert.ErrorIs(t, err, io.EOF)
}

// TestReadFrame_OversizeRejected verifies a hostile guest cannot demand an
// unbounded host allocation via a giant frame length.
func TestReadFrame_OversizeRejected(t *testing.T) {
	var hdr [5]byte
	hdr[0] = FrameStdout
	binary.BigEndian.PutUint32(hdr[1:], MaxFrameBytes+1)
	_, _, err := ReadFrame(bytes.NewReader(hdr[:]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap")
}

// TestReadFrame_TruncatedBody verifies a header that promises more bytes
// than follow fails closed.
func TestReadFrame_TruncatedBody(t *testing.T) {
	var hdr [5]byte
	hdr[0] = FrameStdout
	binary.BigEndian.PutUint32(hdr[1:], 50)
	_, _, err := ReadFrame(bytes.NewReader(hdr[:]))
	assert.Error(t, err)
}

// failWriter fails after allowing n bytes through, exercising the
// write-error branches of WriteRequest and WriteFrame.
type failWriter struct{ remaining int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return len(p), nil
}

// TestWriteErrorsSurface verifies a broken connection surfaces on both the
// length/header write and the payload write, for requests and frames.
func TestWriteErrorsSurface(t *testing.T) {
	req := model.ExecRequest{Command: []string{"sh", "-c", "echo hi"}}
	t.Run("request_length_write", func(t *testing.T) {
		assert.Error(t, WriteRequest(&failWriter{remaining: 0}, req))
	})
	t.Run("request_payload_write", func(t *testing.T) {
		assert.Error(t, WriteRequest(&failWriter{remaining: 4}, req)) // header ok, payload fails
	})
	t.Run("frame_header_write", func(t *testing.T) {
		assert.Error(t, WriteFrame(&failWriter{remaining: 0}, FrameStdout, []byte("x")))
	})
	t.Run("frame_payload_write", func(t *testing.T) {
		assert.Error(t, WriteFrame(&failWriter{remaining: 5}, FrameStdout, []byte("payload"))) // header ok, payload fails
	})
}
