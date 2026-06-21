package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConnectFraming_RoundTrip verifies a request encodes and decodes
// faithfully. Refs: MGIT-11.7.2
func TestConnectFraming_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	req := ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}
	require.NoError(t, EncodeConnectRequest(&buf, req))
	got, err := DecodeConnectRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, req, got)
}

// TestEncodeConnectRequest_Oversized rejects a host that overflows the frame
// ceiling before it reaches the wire. Refs: SEC-04
func TestEncodeConnectRequest_Oversized(t *testing.T) {
	req := ConnectRequest{Protocol: "tcp", Host: strings.Repeat("a", maxConnectRequestBytes), Port: 443}
	err := EncodeConnectRequest(io.Discard, req)
	assert.Error(t, err, "an oversized request is refused at the encoder")
}

// TestDecodeConnectRequest_Errors covers the decoder's failure paths: a
// truncated header, a zero-length frame, an oversized announced frame, and
// malformed JSON. Refs: SEC-04
func TestDecodeConnectRequest_Errors(t *testing.T) {
	t.Run("short_header", func(t *testing.T) {
		_, err := DecodeConnectRequest(bytes.NewReader([]byte{0x01}))
		assert.Error(t, err)
	})
	t.Run("zero_length", func(t *testing.T) {
		_, err := DecodeConnectRequest(bytes.NewReader([]byte{0x00, 0x00}))
		assert.Error(t, err)
	})
	t.Run("truncated_payload", func(t *testing.T) {
		_, err := DecodeConnectRequest(bytes.NewReader([]byte{0x00, 0x10, 'a', 'b'}))
		assert.Error(t, err)
	})
	t.Run("bad_json", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write([]byte{0x00, 0x03})
		buf.WriteString("{ x")
		_, err := DecodeConnectRequest(&buf)
		assert.Error(t, err)
	})
}

// TestDecodeConnectReply_Errors covers the reply decoder's failure paths.
// Refs: SEC-04
func TestDecodeConnectReply_Errors(t *testing.T) {
	t.Run("short_header", func(t *testing.T) {
		_, _, err := DecodeConnectReply(bytes.NewReader([]byte{0x01, 0x00}))
		assert.Error(t, err)
	})
	t.Run("truncated_reason", func(t *testing.T) {
		_, _, err := DecodeConnectReply(bytes.NewReader([]byte{0x00, 0x00, 0x05, 'a'}))
		assert.Error(t, err)
	})
	t.Run("deny_with_reason_round_trip", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, encodeReply(&buf, false, "denied range: link-local"))
		allow, reason, err := DecodeConnectReply(&buf)
		require.NoError(t, err)
		assert.False(t, allow)
		assert.Equal(t, "denied range: link-local", reason)
	})
}

// TestProxy_UpstreamDialFailure verifies a dial error after authorization
// yields a deny reply, not a hang. Refs: SEC-04, MGIT-11.7.2
func TestProxy_UpstreamDialFailure(t *testing.T) {
	rec := &dialRecorder{dialErr: errors.New("connection refused")}
	p := testProxy(t, []string{"registry.npmjs.org"}, resolvesTo("140.82.112.3"), rec.dial)

	guest, host := net.Pipe()
	defer func() { _ = guest.Close() }()
	go p.handle(context.Background(), host)

	require.NoError(t, EncodeConnectRequest(guest, ConnectRequest{Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, reason, err := DecodeConnectReply(guest)
	require.NoError(t, err)
	assert.False(t, allow, "an upstream dial failure is reported as a deny")
	assert.Contains(t, reason, "dial")
}
