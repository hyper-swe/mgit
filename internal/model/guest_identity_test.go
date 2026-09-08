package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A guest identity is a uid/gid pair the guest materializes a name and a
// home for. The daemon fills it from its own uid/gid — what it delivered the
// tree and the base as — so the shape is validated, never the value.
// Refs: MGIT-151
func TestGuestIdentity_Validate(t *testing.T) {
	tests := []struct {
		name    string
		id      GuestIdentity
		wantErr string
	}{
		{name: "host_uid_named_with_home", id: GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}},
		{name: "root", id: RootIdentity()},
		{name: "bare_uid_gid", id: GuestIdentity{UID: 1000, GID: 1000}},
		{name: "negative_uid", id: GuestIdentity{UID: -1, GID: 0}, wantErr: "uid"},
		{name: "negative_gid", id: GuestIdentity{UID: 1, GID: -5}, wantErr: "gid"},
		{name: "name_with_space", id: GuestIdentity{UID: 1, GID: 1, Name: "an agent"}, wantErr: "name"},
		{name: "name_starting_with_digit", id: GuestIdentity{UID: 1, GID: 1, Name: "1agent"}, wantErr: "name"},
		{name: "relative_home", id: GuestIdentity{UID: 1, GID: 1, Home: "home/agent"}, wantErr: "home"},
		{name: "home_with_dotdot", id: GuestIdentity{UID: 1, GID: 1, Home: "/home/../etc"}, wantErr: "home"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.id.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			var verr *ValidationError
			require.ErrorAs(t, err, &verr)
			assert.Equal(t, tt.wantErr, verr.Field)
		})
	}
}

func TestGuestIdentity_IsRoot(t *testing.T) {
	assert.True(t, RootIdentity().IsRoot())
	assert.True(t, GuestIdentity{UID: 0, GID: 20}.IsRoot(), "uid 0 is root whatever the gid")
	assert.False(t, GuestIdentity{UID: 501, GID: 0}.IsRoot(), "gid 0 alone is not root")
}

// An explicit escalation and a non-root identity on the same request is a
// contradiction the daemon must never forward to a guest. Refs: MGIT-151
func TestExecRequest_Validate_IdentityFields(t *testing.T) {
	agent := &GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	root := RootIdentity()
	tests := []struct {
		name    string
		req     ExecRequest
		wantErr string
	}{
		{name: "no_identity_is_the_guest_default", req: ExecRequest{Command: []string{"id"}}},
		{name: "unprivileged_identity", req: ExecRequest{Command: []string{"id"}, RunAs: agent}},
		{name: "as_root_alone", req: ExecRequest{Command: []string{"id"}, AsRoot: true}},
		{name: "as_root_with_root_identity", req: ExecRequest{Command: []string{"id"}, AsRoot: true, RunAs: &root}},
		{name: "as_root_with_unprivileged_identity_is_a_contradiction", req: ExecRequest{Command: []string{"id"}, AsRoot: true, RunAs: agent}, wantErr: "run_as"},
		{name: "invalid_identity_is_nested", req: ExecRequest{Command: []string{"id"}, RunAs: &GuestIdentity{UID: -1}}, wantErr: "run_as.uid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			var verr *ValidationError
			require.ErrorAs(t, err, &verr)
			assert.Equal(t, tt.wantErr, verr.Field)
		})
	}
}

// The identity fields are absent from the wire when unset, so a request
// from this daemon to a guest that predates them decodes as it always did,
// and a result from such a guest reads as "did not say" (nil), never as
// root or as any identity. Refs: MGIT-151
func TestExecRequest_JSON_IdentityFieldsAbsentWhenUnset(t *testing.T) {
	b, err := json.Marshal(ExecRequest{Command: []string{"id"}})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "run_as")
	assert.NotContains(t, string(b), "as_root")

	b, err = json.Marshal(ExecResult{ExitCode: 3})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "ran_as")

	var res ExecResult
	require.NoError(t, json.Unmarshal([]byte(`{"stdout":null,"stderr":null,"exit_code":0}`), &res))
	assert.Nil(t, res.RanAs, "a guest that did not say leaves RanAs nil")

	var req ExecRequest
	require.NoError(t, json.Unmarshal([]byte(`{"command":["id"],"run_as":{"uid":501,"gid":20,"name":"agent","home":"/home/agent"},"as_root":false}`), &req))
	require.NotNil(t, req.RunAs)
	assert.Equal(t, GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}, *req.RunAs)
}
