package guest

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// rwPair pairs a reader (request bytes) with a writer (captured
// response) as one io.ReadWriter for Serve.
type rwPair struct {
	io.Reader
	io.Writer
}

// TestServe_MalformedRequests verifies Serve fails closed on bad
// framing and surfaces the error in the result frame.
func TestServe_MalformedRequests(t *testing.T) {
	sup := testSupervisor(t)
	ctx := context.Background()

	t.Run("truncated_length_header", func(t *testing.T) {
		var resp bytes.Buffer
		err := sup.Serve(ctx, rwPair{Reader: strings.NewReader("\x00\x00"), Writer: &resp})
		assert.Error(t, err, "a truncated header is an error")
	})

	t.Run("oversized_request_rejected", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], maxExecRequestBytes+1)
		var resp bytes.Buffer
		err := sup.Serve(ctx, rwPair{Reader: bytes.NewReader(hdr[:]), Writer: &resp})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cap", "the request cap is enforced")
		_, _, outcome := readResponse(t, &resp)
		assert.Zero(t, outcome.ExitCode)
	})

	t.Run("malformed_json_request", func(t *testing.T) {
		payload := []byte("{not json")
		var buf bytes.Buffer
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
		buf.Write(hdr[:])
		buf.Write(payload)

		var resp bytes.Buffer
		err := sup.Serve(ctx, rwPair{Reader: &buf, Writer: &resp})
		require.Error(t, err)
		// The error is still reported back in a result frame.
		_, _, outcome := readResponse(t, &resp)
		assert.Zero(t, outcome.ExitCode)
	})

	t.Run("invalid_command_reported_in_result", func(t *testing.T) {
		var req bytes.Buffer
		require.NoError(t, writeRequest(&req, model.ExecRequest{Command: []string{}}))
		var resp bytes.Buffer
		err := sup.Serve(ctx, rwPair{Reader: &req, Writer: &resp})
		require.Error(t, err, "empty command surfaces as a serve error")
		stdout, stderr, _ := readResponse(t, &resp)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})
}

// failWriter fails after allowing n bytes through, exercising the
// frame/result write-error paths.
type failWriter struct {
	remaining int
}

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

// TestServe_WriteFailuresSurface verifies a broken response connection
// surfaces rather than being swallowed.
func TestServe_WriteFailuresSurface(t *testing.T) {
	sup := testSupervisor(t)
	ctx := context.Background()

	t.Run("result_frame_write_fails", func(t *testing.T) {
		var req bytes.Buffer
		require.NoError(t, writeRequest(&req, model.ExecRequest{Command: []string{"/bin/echo", "hi"}}))
		// Allow the stdout frame through, then fail on the result frame.
		err := sup.Serve(ctx, rwPair{Reader: &req, Writer: &failWriter{remaining: 8}})
		assert.Error(t, err, "a failed result write surfaces")
	})

	t.Run("stdout_frame_write_fails", func(t *testing.T) {
		var req bytes.Buffer
		require.NoError(t, writeRequest(&req, model.ExecRequest{Command: []string{"/bin/echo", "hi"}}))
		err := sup.Serve(ctx, rwPair{Reader: &req, Writer: &failWriter{remaining: 0}})
		assert.Error(t, err)
	})
}

// TestServe_LargeOutputStreams verifies large stdout is delivered
// across many frames intact.
func TestServe_LargeOutputStreams(t *testing.T) {
	sup := testSupervisor(t)
	var req bytes.Buffer
	require.NoError(t, writeRequest(&req, model.ExecRequest{
		Command: []string{"/bin/sh", "-c", "for i in $(seq 1 1000); do echo line-$i; done"},
	}))

	var resp bytes.Buffer
	require.NoError(t, sup.Serve(context.Background(), rwPair{Reader: &req, Writer: &resp}))

	stdout, _, outcome := readResponse(t, &resp)
	assert.Zero(t, outcome.ExitCode)
	assert.Contains(t, stdout, "line-1\n")
	assert.Contains(t, stdout, "line-1000\n")
	assert.Equal(t, 1000, strings.Count(stdout, "\n"))
}
