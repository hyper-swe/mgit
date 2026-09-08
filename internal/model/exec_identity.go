package model

import "fmt"

// ExecIdentity is the daemon's verdict on the identity a command ran as:
// what it asked the guest for, what the guest reported, and whether the two
// agree. It rides the exec result to the operator so that "the guest ran
// this as root" is never silent. Refs: MGIT-151
type ExecIdentity struct {
	Asked    *GuestIdentity `json:"asked,omitempty"`  // what the daemon requested; nil = the guest's default
	Ran      *GuestIdentity `json:"ran,omitempty"`    // what the guest reported; nil = it did not say
	Verified bool           `json:"verified"`         // Ran confirms Asked by uid and gid
	Reason   string         `json:"reason,omitempty"` // why not, in the operator's words
}

// VerdictOnExecIdentity judges an echoed identity against the one asked
// for. Only an exact uid+gid echo verifies; a guest that did not say is
// unverified (it predates the field and runs commands as root); nothing
// asked is unverifiable by construction. Refs: MGIT-151
func VerdictOnExecIdentity(asked, ran *GuestIdentity) ExecIdentity {
	v := ExecIdentity{Asked: asked, Ran: ran}
	switch {
	case asked == nil:
		v.Reason = "no identity was asked for, so the guest's default ran (root on guests that predate this field)"
	case ran == nil:
		v.Reason = fmt.Sprintf("the guest did not report the identity it ran as; asked for uid %d gid %d — "+
			"a base composed before this version, or a backend that does not switch identities (container), "+
			"runs commands as root; recompose the base with `mgit sandbox base from <image>` or use a microVM backend",
			asked.UID, asked.GID)
	case ran.UID != asked.UID || ran.GID != asked.GID:
		v.Reason = fmt.Sprintf("the guest ran as uid %d gid %d, not the uid %d gid %d asked for",
			ran.UID, ran.GID, asked.UID, asked.GID)
	default:
		v.Verified = true
	}
	return v
}
