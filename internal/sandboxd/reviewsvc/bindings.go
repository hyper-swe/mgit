package reviewsvc

import (
	"context"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// AttachInput is what a Bindings implementation is given: the guest that was
// provisioned and sealed, the verified attestation, and the operator's mapping.
// It carries identifiers only; a credential is named by Mapping.CredentialRef
// and an egress policy by Mapping.EgressPolicyID, never by value.
type AttachInput struct {
	GuestID     string
	LeaseDir    string
	Attestation reviewprep.Attestation
	Mapping     reviewprep.Mapping
	// Exec runs a command in this guest as root, before the reviewer starts. It
	// is the only way an implementation reaches the guest. It is for setup, not
	// for secrets: an argument or an environment variable of an exec is visible
	// to the guest's other processes, so a credential must not travel in one.
	Exec func(ctx context.Context, req model.ExecRequest) (*model.ExecResult, error)
	// AllowEgress admits one exact ip:port (never a host name) for this guest
	// on the host's egress stack, before the reviewer starts. It is nil when
	// the guest has no network: the mapping names no egress policy, or this
	// daemon has no stack. An implementation that needs egress and finds it nil
	// refuses. Every grant is revoked when the guest is removed. Refs: MGIT-344
	AllowEgress func(ctx context.Context, entry string) error
}

// Bindings is the seam between running a review and what a review needs beyond
// the sealed bundle: the binding map that says what the reviewer may refer to,
// the network policy, and the credential slot.
//
// Attach is called once per run, after the guest is provisioned and its seal
// verified and before the reviewer starts. It installs whatever the mapping asks
// for and returns a release function. The release function is never nil, on any
// return path including an error, and the caller always calls it, whatever
// happens and without checking it first; calling it twice is safe. An error from Attach is a
// refusal: *reviewprep.Error, with the reviewer not started. An implementation
// must refuse what it cannot provide rather than run without it.
type Bindings interface {
	Attach(ctx context.Context, in AttachInput) (release func(context.Context) error, err error)
}

// NoBindings is the default: nothing is installed. It grants no network and no
// credential, so a mapping that asks for either is refused instead of run
// without it.
type NoBindings struct{}

// Attach implements Bindings.
func (NoBindings) Attach(_ context.Context, in AttachInput) (func(context.Context) error, error) {
	if in.Mapping.CredentialRef != "" {
		return noRelease, reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "this build provides no credential slot, and the mapping asks for one")
	}
	if p := in.Mapping.EgressPolicyID; p != "" && p != "none" {
		return noRelease, reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "this build provides no network policy, and the mapping asks for one")
	}
	return noRelease, nil
}

// noRelease is the release function of an Attach that installed nothing. It is
// returned on every path, refusals included, so a caller can always call it.
func noRelease(context.Context) error { return nil }
