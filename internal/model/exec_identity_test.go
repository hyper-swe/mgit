package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The daemon judges what the guest reported against what it asked, and
// says so in the operator's words: a guest that did not say is
// UNVERIFIED (a base composed before the field runs commands as root), a
// guest that ran as something else is a MISMATCH, and only an exact
// uid+gid echo is verified. Refs: MGIT-151
func TestVerdictOnExecIdentity(t *testing.T) {
	agent := GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	root := RootIdentity()
	tests := []struct {
		name         string
		asked, ran   *GuestIdentity
		wantVerified bool
		wantReason   string
	}{
		{name: "exact_echo", asked: &agent, ran: &agent, wantVerified: true},
		{name: "echo_differs_only_in_name_and_home", asked: &agent, ran: &GuestIdentity{UID: 501, GID: 20}, wantVerified: true},
		{name: "guest_did_not_say", asked: &agent, ran: nil, wantReason: "did not report"},
		{name: "guest_ran_as_root_instead", asked: &agent, ran: &root, wantReason: "ran as uid 0 gid 0, not the uid 501 gid 20 asked for"},
		{name: "gid_differs", asked: &agent, ran: &GuestIdentity{UID: 501, GID: 0}, wantReason: "ran as uid 501 gid 0"},
		{name: "root_asked_root_ran", asked: &root, ran: &root, wantVerified: true},
		{name: "nothing_asked_is_the_guest_default_and_unverifiable", asked: nil, ran: &root, wantReason: "no identity was asked for"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := VerdictOnExecIdentity(tt.asked, tt.ran)
			assert.Equal(t, tt.wantVerified, v.Verified)
			assert.Equal(t, tt.asked, v.Asked)
			assert.Equal(t, tt.ran, v.Ran)
			if tt.wantVerified {
				assert.Empty(t, v.Reason)
			} else {
				assert.Contains(t, v.Reason, tt.wantReason)
			}
		})
	}
}
