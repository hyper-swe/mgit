package reviewsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

func attach(t *testing.T, b Bindings, m reviewprep.Mapping) (func(context.Context) error, error) {
	t.Helper()
	return b.Attach(context.Background(), AttachInput{GuestID: "g1", LeaseDir: "/lease", Mapping: m, Attestation: reviewprep.Attestation{Lease: "l"}})
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatalf("want a refusal envelope, got %v", err)
	}
	return e.Code
}

// Nothing installed behind the seam means nothing is granted: a review that asks
// for a credential or for any network is refused, never run without them.
func TestNoBindings_AllowsAMappingThatAsksForNothing(t *testing.T) {
	for _, egress := range []string{"", "none"} {
		release, err := attach(t, NoBindings{}, reviewprep.Mapping{EgressPolicyID: egress})
		if err != nil || release == nil {
			t.Fatalf("egress %q: release=%v err=%v", egress, release == nil, err)
		}
		if err := release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoBindings_RefusesACredentialItCannotProvide(t *testing.T) {
	_, err := attach(t, NoBindings{}, reviewprep.Mapping{CredentialRef: "cred-1", EgressPolicyID: "none"})
	if err == nil || refusalCode(t, err) != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("err=%v", err)
	}
}

func TestNoBindings_RefusesAnEgressPolicyItCannotEnforce(t *testing.T) {
	_, err := attach(t, NoBindings{}, reviewprep.Mapping{EgressPolicyID: "allow-docs"})
	if err == nil || refusalCode(t, err) != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("err=%v", err)
	}
}

func TestNoBindings_TheRefusalSaysNothingSecret(t *testing.T) {
	_, err := attach(t, NoBindings{}, reviewprep.Mapping{CredentialRef: "super-secret-ref-123"})
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	if e.ReviewerStarted || contains(e.Detail, "super-secret-ref-123") {
		t.Fatalf("envelope %+v", e)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
