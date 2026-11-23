package reviewsvc

import (
	"context"
	"errors"
	"strings"
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

// The operator's map is read through a loader the service is given, so the
// package that owns map loading can replace it without editing the service.
func TestService_UsesTheMapLoaderItIsGiven(t *testing.T) {
	e := setup(t)
	var gotRoot, gotID string
	e.svc.WithMapLoader(func(hostRoot, id string) (*reviewprep.Mapping, error) {
		gotRoot, gotID = hostRoot, id
		return nil, reviewprep.Refuse(reviewprep.CodeMappingMismatch, "from the loader")
	})
	_, err := e.svc.PrepareReview(context.Background(), e.args)
	if err == nil || refusalCode(t, err) != reviewprep.CodeMappingMismatch {
		t.Fatalf("err=%v", err)
	}
	if gotRoot != e.root || gotID != e.args.MappingID {
		t.Fatalf("loader called with %q %q", gotRoot, gotID)
	}
}

// A replacement loader sees only ids the service has already accepted: the
// check that keeps an id from naming a path lives in the service, ahead of any
// loader, not inside the default one.
func TestService_NoLoaderIsEverHandedAnIdThatCouldNameAPath(t *testing.T) {
	ids := []string{"../etc/passwd", "..", ".", "a/b", "/abs", "a\x00b", "", strings.Repeat("a", 65), ".hidden", "a b", "a\\b"}
	for _, id := range ids {
		e := setup(t)
		called := false
		e.svc.WithMapLoader(func(string, string) (*reviewprep.Mapping, error) {
			called = true
			return nil, reviewprep.Refuse(reviewprep.CodeMappingMismatch, "loader")
		})
		e.args.MappingID = id
		_, err := e.svc.PrepareReview(context.Background(), e.args)
		if called {
			t.Fatalf("id %q reached the loader", id)
		}
		if err == nil || refusalCode(t, err) != reviewprep.CodeMappingMismatch {
			t.Fatalf("id %q: err=%v", id, err)
		}
	}
	// A well-formed id does reach it.
	e := setup(t)
	called := false
	e.svc.WithMapLoader(func(string, string) (*reviewprep.Mapping, error) {
		called = true
		return nil, reviewprep.Refuse(reviewprep.CodeMappingMismatch, "loader")
	})
	_, _ = e.svc.PrepareReview(context.Background(), e.args)
	if !called {
		t.Fatal("a valid id never reached the loader")
	}
}

// The caller always calls the release function, including after an error, and
// does not check it first: every return path of Attach hands back one that is
// safe to call.
func TestNoBindings_ReleaseIsNeverNilAndSafeToCallOnEveryPath(t *testing.T) {
	cases := map[string]reviewprep.Mapping{
		"nothing asked":   {},
		"credential":      {CredentialRef: "cred-1"},
		"network policy":  {EgressPolicyID: "allow-docs"},
		"both":            {CredentialRef: "cred-1", EgressPolicyID: "allow-docs"},
		"none is allowed": {EgressPolicyID: "none"},
	}
	for name, m := range cases {
		release, _ := attach(t, NoBindings{}, m)
		if release == nil {
			t.Fatalf("%s: release is nil", name)
		}
		if err := release(context.Background()); err != nil {
			t.Fatalf("%s: release: %v", name, err)
		}
		if err := release(context.Background()); err != nil { // twice is fine
			t.Fatalf("%s: second release: %v", name, err)
		}
	}
}
