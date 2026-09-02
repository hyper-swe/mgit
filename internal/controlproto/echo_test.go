package controlproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEcho_BuildsAResponseOfExactlyTheRequestedSize pins the property the
// doctor check rests on: the daemon can produce a response whose encoded size
// is EXACTLY what was asked for, so "the full cap" means the cap and not an
// estimate of it. The sizes come from the contract, not the code: a small
// answer, the two edges of the cap, and one byte over it — which the daemon
// must be able to BUILD, because WriteResponse is the layer that refuses it.
// Refs: MGIT-175, MGIT-160
func TestEcho_BuildsAResponseOfExactlyTheRequestedSize(t *testing.T) {
	for _, n := range []int{512, 4096, MaxResponseBytes - 1, MaxResponseBytes, MaxResponseBytes + 1} {
		resp, err := BuildEchoResponse(n)
		require.NoError(t, err, "size %d", n)
		payload, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.Len(t, payload, n, "the encoded response must be exactly %d bytes", n)
		assert.Equal(t, n, resp.Echo.Bytes)
		assert.NoError(t, VerifyEcho(resp.Echo), "a freshly built echo verifies")
	}
}

// A request smaller than the response envelope itself cannot be honored
// exactly, and is refused rather than rounded up. Refs: MGIT-175
func TestEcho_SmallerThanTheEnvelope_IsRefused(t *testing.T) {
	_, err := BuildEchoResponse(10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "envelope")
}

// TestEcho_Validate_FailsClosed: the daemon supervises every VM, so a crafted
// echo must never drive an allocation past the stated bound. Refs: MGIT-175
func TestEcho_Validate_FailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		req     *Request
		wantErr bool
	}{
		{"missing_args", &Request{Kind: KindEcho}, true},
		{"negative", &Request{Kind: KindEcho, Echo: &EchoArgs{Bytes: -1}}, true},
		{"over_the_echo_bound", &Request{Kind: KindEcho, Echo: &EchoArgs{Bytes: MaxEchoBytes + 1}}, true},
		{"at_the_echo_bound", &Request{Kind: KindEcho, Echo: &EchoArgs{Bytes: MaxEchoBytes}}, false},
		{"the_full_cap", &Request{Kind: KindEcho, Echo: &EchoArgs{Bytes: MaxResponseBytes}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestEcho_VerifyEcho_RefusesATamperedAnswer is the negative control for the
// integrity half: a verifier that accepted any fill would make "arrived
// intact" mean "arrived". Refs: MGIT-175
func TestEcho_VerifyEcho_RefusesATamperedAnswer(t *testing.T) {
	resp, err := BuildEchoResponse(4096)
	require.NoError(t, err)
	good := *resp.Echo

	flipped := good
	flipped.Fill = "b" + good.Fill[1:]
	assert.Error(t, VerifyEcho(&flipped), "a changed byte must fail the digest")

	short := good
	short.Fill = good.Fill[:len(good.Fill)-1]
	assert.Error(t, VerifyEcho(&short), "a shorter fill must fail")

	resized := good
	resized.Bytes++
	assert.Error(t, VerifyEcho(&resized), "a size claim the payload does not match must fail")

	assert.NoError(t, VerifyEcho(&good), "control: the untouched answer verifies")
	assert.Error(t, VerifyEcho(nil), "no answer is not an intact answer")
}

// TestEcho_RoundTripsThroughTheCodec exercises the real frames: the request
// kind survives the wire, a full-cap response is written and read back
// intact, and one byte over is refused by WriteResponse with the MGIT-160
// sentinel — the exact mechanism the doctor check provokes. Refs: MGIT-175
func TestEcho_RoundTripsThroughTheCodec(t *testing.T) {
	var reqBuf bytes.Buffer
	require.NoError(t, WriteRequest(&reqBuf, &Request{Kind: KindEcho, Echo: &EchoArgs{Bytes: MaxResponseBytes}}))
	req, err := ReadRequest(&reqBuf)
	require.NoError(t, err)
	assert.Equal(t, KindEcho, req.Kind)
	require.NotNil(t, req.Echo)
	assert.Equal(t, MaxResponseBytes, req.Echo.Bytes)

	full, err := BuildEchoResponse(MaxResponseBytes)
	require.NoError(t, err)
	var respBuf bytes.Buffer
	require.NoError(t, WriteResponse(&respBuf, full))
	got, err := ReadResponse(&respBuf)
	require.NoError(t, err)
	require.NotNil(t, got.Echo)
	assert.NoError(t, VerifyEcho(got.Echo))

	over, err := BuildEchoResponse(MaxResponseBytes + 1)
	require.NoError(t, err)
	err = WriteResponse(&respBuf, over)
	require.True(t, errors.Is(err, ErrResponseTooLarge), "one byte over the cap is the MGIT-160 refusal: %v", err)
	assert.True(t, strings.Contains(err.Error(), "Narrow what you asked for"), "the refusal carries its remedy")
}
