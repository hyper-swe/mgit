package reviewsvc

import (
	"context"

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
}

// Bindings is the seam between running a review and what a review needs beyond
// the sealed bundle: the binding map that says what the reviewer may refer to,
// the network policy, and the credential slot.
//
// Attach is called once per run, after the guest is provisioned and its seal
// verified and before the reviewer starts. It installs whatever the mapping asks
// for and returns a release function, which the run calls whatever happens
// (including when Attach's own later steps fail). An error from Attach is a
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
		return nil, reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "this build provides no credential slot, and the mapping asks for one")
	}
	if p := in.Mapping.EgressPolicyID; p != "" && p != "none" {
		return nil, reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "this build provides no network policy, and the mapping asks for one")
	}
	return func(context.Context) error { return nil }, nil
}
