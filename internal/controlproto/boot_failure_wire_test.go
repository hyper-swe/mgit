package controlproto

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The record rides in the status and list responses, which the client
// decodes with DisallowUnknownFields at ProtocolVersion 5. Carried through
// the real frame writer and reader, it arrives intact. Refs: MGIT-231.1, MGIT-231
func TestResponse_LastBootFailure_CrossesTheStrictDecode(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 21, 3, 0, time.UTC)
	sent := &Response{
		Sandbox: &model.SandboxInfo{ID: "01JSB", TaskID: "T-1", State: model.StateCreated,
			LastBootFailure: &model.BootFailure{At: at, Cause: "kvm launch: create vm: boom"}},
		List: []model.SandboxInfo{{ID: "01JSB", TaskID: "T-1", LastBootFailure: &model.BootFailure{At: at, Cause: "boom"}}},
	}
	var buf bytes.Buffer
	require.NoError(t, WriteResponse(&buf, sent))
	got, err := ReadResponse(&buf)
	require.NoError(t, err)
	require.NotNil(t, got.Sandbox.LastBootFailure)
	assert.Equal(t, at, got.Sandbox.LastBootFailure.At.UTC())
	assert.Equal(t, "kvm launch: create vm: boom", got.Sandbox.LastBootFailure.Cause)
	require.Len(t, got.List, 1)
	assert.Equal(t, "boom", got.List[0].LastBootFailure.Cause)
}
