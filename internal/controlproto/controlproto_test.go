package controlproto

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func validLaunch() *Request {
	return &Request{Kind: KindLaunch, Launch: &model.SandboxLaunchOptions{
		TaskID: "MGIT-1", WorktreePath: "/work/a",
		ImageRef: "img@sha256:" + strings.Repeat("a", 64),
		Network:  model.NetworkPolicy{Mode: model.NetworkModeNone},
	}}
}

// TestControlProto_RequestRoundTrip verifies every request kind survives
// write->read with its payload intact.
func TestControlProto_RequestRoundTrip(t *testing.T) {
	reqs := map[string]*Request{
		"launch": validLaunch(),
		"exec":   {Kind: KindExec, Exec: &ExecArgs{TaskID: "MGIT-1", Exec: model.ExecRequest{Command: []string{"sh", "-c", "echo hi"}}}},
		"land":   {Kind: KindLand, Land: &TaskRef{TaskID: "MGIT-1"}},
		"list":   {Kind: KindList},
		"remove": {Kind: KindRemove, Remove: &RemoveArgs{TaskID: "MGIT-1", Force: true}},
		"status": {Kind: KindStatus, Status: &TaskRef{TaskID: "MGIT-1"}},
	}
	for name, req := range reqs {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteRequest(&buf, req))
			got, err := ReadRequest(&buf)
			require.NoError(t, err)
			assert.Equal(t, req.Kind, got.Kind)
			assert.Equal(t, req, got)
		})
	}
}

// TestControlProto_ResponseRoundTrip covers the response codec.
func TestControlProto_ResponseRoundTrip(t *testing.T) {
	resp := &Response{List: []model.SandboxInfo{{ID: "sbx-1", TaskID: "MGIT-1", State: model.StateRunning}}}
	var buf bytes.Buffer
	require.NoError(t, WriteResponse(&buf, resp))
	got, err := ReadResponse(&buf)
	require.NoError(t, err)
	require.Len(t, got.List, 1)
	assert.Equal(t, "sbx-1", got.List[0].ID)
}

// TestControlProto_OversizeRejected verifies a declared length over the
// cap is refused before allocation.
func TestControlProto_OversizeRejected(t *testing.T) {
	var hdr [frameHeaderLen]byte
	hdr[0] = KindList
	binary.BigEndian.PutUint32(hdr[1:], MaxRequestBytes+1)
	_, err := ReadRequest(bytes.NewReader(hdr[:]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap")
}

// TestControlProto_TruncatedFailsClosed verifies truncated framing fails
// closed (header short, and body shorter than declared).
func TestControlProto_TruncatedFailsClosed(t *testing.T) {
	t.Run("short_header", func(t *testing.T) {
		_, err := ReadRequest(bytes.NewReader([]byte{KindList, 0x00}))
		assert.Error(t, err)
	})
	t.Run("short_body", func(t *testing.T) {
		var hdr [frameHeaderLen]byte
		hdr[0] = KindList
		binary.BigEndian.PutUint32(hdr[1:], 50) // claims 50, supplies none
		_, err := ReadRequest(bytes.NewReader(hdr[:]))
		assert.Error(t, err)
	})
}

// TestControlProto_UnknownKindRejected verifies an unknown frame tag is
// refused.
func TestControlProto_UnknownKindRejected(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeFrame(&buf, 'Z', []byte("{}")))
	_, err := ReadRequest(&buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown request kind")
}

// TestControlProto_UnknownJSONField_Rejected verifies DisallowUnknownFields.
func TestControlProto_UnknownJSONField_Rejected(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeFrame(&buf, KindList, []byte(`{"bogus":1}`)))
	_, err := ReadRequest(&buf)
	assert.Error(t, err)
}

// TestControlProto_PayloadKindMismatch_Rejected verifies a request whose
// payload does not match its kind is malformed (defense-in-depth).
func TestControlProto_PayloadKindMismatch_Rejected(t *testing.T) {
	var buf bytes.Buffer
	// Frame tagged List but carrying a launch payload.
	require.NoError(t, writeFrame(&buf, KindList, []byte(`{"launch":{"task_id":"x"}}`)))
	_, err := ReadRequest(&buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "payload")
}

// TestControlProto_ExecBounds verifies argv/env ceilings reject oversized
// exec requests before they reach the guest.
func TestControlProto_ExecBounds(t *testing.T) {
	t.Run("argv_over_cap", func(t *testing.T) {
		req := &Request{Kind: KindExec, Exec: &ExecArgs{TaskID: "T", Exec: model.ExecRequest{Command: make([]string, MaxArgv+1)}}}
		assert.Error(t, req.Validate())
	})
	t.Run("env_over_cap", func(t *testing.T) {
		req := &Request{Kind: KindExec, Exec: &ExecArgs{TaskID: "T", Exec: model.ExecRequest{
			Command: []string{"sh"}, Env: make([]string, MaxEnv+1)}}}
		assert.Error(t, req.Validate())
	})
	t.Run("env_entry_too_long", func(t *testing.T) {
		req := &Request{Kind: KindExec, Exec: &ExecArgs{TaskID: "T", Exec: model.ExecRequest{
			Command: []string{"sh"}, Env: []string{strings.Repeat("x", MaxEnvEntryBytes+1)}}}}
		assert.Error(t, req.Validate())
	})
}

// TestControlProto_MissingPayload_Rejected verifies each kind requires its
// payload / task id.
func TestControlProto_MissingPayload_Rejected(t *testing.T) {
	for _, req := range []*Request{
		{Kind: KindLaunch},
		{Kind: KindExec},
		{Kind: KindLand},
		{Kind: KindRemove},
		{Kind: KindStatus},
	} {
		assert.Error(t, req.Validate(), "kind %#x with no payload must be rejected", req.Kind)
	}
}

// FuzzReadRequest asserts the decoder never panics on arbitrary bytes
// (Security Audit V2: the control-plane decoder is an untrusted-input
// parser on the single daemon).
func FuzzReadRequest(f *testing.F) {
	f.Add([]byte{KindList, 0, 0, 0, 2, '{', '}'})
	f.Add([]byte{KindExec, 0, 0, 0, 0})
	f.Add([]byte{'Z', 0xff, 0xff, 0xff, 0xff})
	var ok bytes.Buffer
	_ = WriteRequest(&ok, validLaunch())
	f.Add(ok.Bytes())
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ReadRequest(bytes.NewReader(data)) // must never panic
	})
}

// failWriter fails after allowing n bytes through, exercising the
// write-error branches.
type failWriter struct{ remaining int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, assert.AnError
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, assert.AnError
	}
	w.remaining -= len(p)
	return len(p), nil
}

// TestControlProto_WriteErrorsSurface verifies broken-connection writes
// surface on both the header and payload, for requests and responses.
func TestControlProto_WriteErrorsSurface(t *testing.T) {
	t.Run("request_header", func(t *testing.T) {
		assert.Error(t, WriteRequest(&failWriter{remaining: 0}, validLaunch()))
	})
	t.Run("request_payload", func(t *testing.T) {
		assert.Error(t, WriteRequest(&failWriter{remaining: frameHeaderLen}, validLaunch()))
	})
	t.Run("response_header", func(t *testing.T) {
		assert.Error(t, WriteResponse(&failWriter{remaining: 0}, &Response{}))
	})
	t.Run("response_payload", func(t *testing.T) {
		assert.Error(t, WriteResponse(&failWriter{remaining: 4}, &Response{Landed: &LandResult{Commits: 1}}))
	})
}

// TestControlProto_ResponseReadErrors covers the response read ceilings.
func TestControlProto_ResponseReadErrors(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], MaxResponseBytes+1)
		_, err := ReadResponse(bytes.NewReader(hdr[:]))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cap")
	})
	t.Run("truncated_body", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 50)
		_, err := ReadResponse(bytes.NewReader(hdr[:]))
		assert.Error(t, err)
	})
	t.Run("short_header", func(t *testing.T) {
		_, err := ReadResponse(bytes.NewReader([]byte{0x00}))
		assert.Error(t, err)
	})
	t.Run("unknown_field", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, writeLenPrefixed(&buf, []byte(`{"bogus":1}`)))
		_, err := ReadResponse(&buf)
		assert.Error(t, err)
	})
}
