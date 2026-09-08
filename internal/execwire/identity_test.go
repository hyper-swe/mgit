package execwire

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The guest echoes the identity it ran the command as in the terminal
// result frame, so the host can verify what it asked for; a frame from a
// guest that predates the field reads as nil, never as an identity.
// Refs: MGIT-151
func TestResultFrame_CarriesTheIdentityTheGuestRanAs(t *testing.T) {
	ran := &model.GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	payload, err := json.Marshal(ResultFrame{Result: Result{ExitCode: 0, RanAs: ran}})
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"ran_as"`)

	var rf ResultFrame
	require.NoError(t, json.Unmarshal(payload, &rf))
	require.NotNil(t, rf.Result.RanAs)
	assert.Equal(t, *ran, *rf.Result.RanAs)

	var old ResultFrame
	require.NoError(t, json.Unmarshal([]byte(`{"outcome":{"exit_code":0,"usage":{"user_time_ns":0,"system_time_ns":0}}}`), &old))
	assert.Nil(t, old.Result.RanAs, "an older guest's frame carries no identity")
}

// The request carries the identity to run as and the escalation flag
// through the length-prefixed request frame unchanged. Refs: MGIT-151
func TestRequest_RoundTripsTheIdentityFields(t *testing.T) {
	var buf bytes.Buffer
	in := model.ExecRequest{
		Command: []string{"id", "-u"},
		RunAs:   &model.GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"},
	}
	require.NoError(t, WriteRequest(&buf, in))
	out, err := ReadRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, in, out)

	buf.Reset()
	require.NoError(t, WriteRequest(&buf, model.ExecRequest{Command: []string{"id"}, AsRoot: true}))
	out, err = ReadRequest(&buf)
	require.NoError(t, err)
	assert.True(t, out.AsRoot)
	assert.Nil(t, out.RunAs)
}
