package reviewprep

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

const imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func fixture(t *testing.T) (Request, Mapping, []byte, *Signer) {
	t.Helper()
	bundle := []byte(`{"citations":[{"n":1,"text":"a line"}]}`)
	m := Mapping{
		Schema:           MapSchema,
		ID:               "pi-v1",
		BindingSHA256:    strings.Repeat("b", 64),
		ImageDigest:      imageDigest,
		GuestEntrypoint:  "/opt/reviewer/bin/reviewer",
		EntrypointSHA256: strings.Repeat("e", 64),
		Argv:             []string{"--json", "--offline"},
		EgressPolicyID:   "none",
	}
	r := Request{
		Profile:       Profile,
		Image:         "reviewer@" + imageDigest,
		MappingID:     "pi-v1",
		BindingDigest: strings.Repeat("b", 64),
		BundleDigest:  sum(bundle),
		Nonce:         "nonce-0123456789abcdef",
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner("mgit-host", priv)
	if err != nil {
		t.Fatal(err)
	}
	return r, m, bundle, s
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %v", err)
	}
	if e.ReviewerStarted {
		t.Fatalf("pre-launch refusal must say reviewer_started=false")
	}
	return e.Code
}

func TestPrepare_Positive_AttestationMatchesRequest(t *testing.T) {
	r, m, b, s := fixture(t)
	att, err := Prepare(r, m, b, s, fixedNow, 5*time.Minute, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	if att.Schema != AttestSchema || att.Nonce != r.Nonce || att.Lease != "lease-1" ||
		att.Profile != Profile || att.ImageDigest != imageDigest || att.MappingID != "pi-v1" ||
		att.BindingSHA256 != r.BindingDigest || att.EntrypointSHA256 != m.EntrypointSHA256 ||
		att.BundleSHA256 != sum(b) || att.Cwd != "/review" || att.EgressPolicyID != "none" {
		t.Fatalf("attestation does not carry the request: %+v", att)
	}
	if want := sum([]byte(`["--json","--offline"]`)); att.ArgvSHA256 != want {
		t.Fatalf("argv digest %s want %s", att.ArgvSHA256, want)
	}
	e := att.Enforced
	if !e.DetachedGuest || e.Bundle != "sealed-ro" || e.FilesystemWrites || e.AdditionalExec || e.HostMounts {
		t.Fatalf("enforced block wrong: %+v", e)
	}
	if !att.ExpiresAt.Equal(fixedNow.Add(5 * time.Minute)) {
		t.Fatalf("expiry %v", att.ExpiresAt)
	}
	if err := Verify(att, r, s.Public(), fixedNow.Add(time.Minute)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestPrepare_Refusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Request, *Mapping, *[]byte)
		code string
	}{
		{"unknown profile", func(r *Request, _ *Mapping, _ *[]byte) { r.Profile = "reviewer-rw/v1" }, "PROFILE_UNAVAILABLE"},
		{"image digest differs from the map", func(r *Request, _ *Mapping, _ *[]byte) {
			r.Image = "reviewer@sha256:" + strings.Repeat("2", 64)
		}, "IMAGE_MISMATCH"},
		{"image without a digest", func(r *Request, _ *Mapping, _ *[]byte) { r.Image = "reviewer:latest" }, "IMAGE_MISMATCH"},
		{"mapping id differs", func(r *Request, _ *Mapping, _ *[]byte) { r.MappingID = "other" }, "MAPPING_MISMATCH"},
		{"binding digest differs", func(r *Request, _ *Mapping, _ *[]byte) { r.BindingDigest = strings.Repeat("c", 64) }, "MAPPING_MISMATCH"},
		{"map schema unknown", func(_ *Request, m *Mapping, _ *[]byte) { m.Schema = "mgit.review-map/v2" }, "MAPPING_MISMATCH"},
		{"bundle bytes differ from the digest", func(_ *Request, _ *Mapping, b *[]byte) { *b = []byte(`{"citations":[]}`) }, "BUNDLE_INVALID"},
		{"bundle is not json", func(r *Request, _ *Mapping, b *[]byte) {
			*b = []byte("not json")
			r.BundleDigest = sum(*b)
		}, "BUNDLE_INVALID"},
		{"bundle empty", func(r *Request, _ *Mapping, b *[]byte) {
			*b = nil
			r.BundleDigest = sum(*b)
		}, "BUNDLE_INVALID"},
		{"entrypoint relative", func(_ *Request, m *Mapping, _ *[]byte) { m.GuestEntrypoint = "bin/reviewer" }, "POLICY_UNENFORCEABLE"},
		{"entrypoint climbs", func(_ *Request, m *Mapping, _ *[]byte) { m.GuestEntrypoint = "/opt/../bin/sh" }, "POLICY_UNENFORCEABLE"},
		{"entrypoint hash missing", func(_ *Request, m *Mapping, _ *[]byte) { m.EntrypointSHA256 = "" }, "POLICY_UNENFORCEABLE"},
		{"credential without egress policy", func(_ *Request, m *Mapping, _ *[]byte) {
			m.CredentialRef = "cred-1"
			m.EgressPolicyID = ""
		}, "POLICY_UNENFORCEABLE"},
		{"nonce empty", func(r *Request, _ *Mapping, _ *[]byte) { r.Nonce = "" }, "ATTESTATION_INVALID"},
		{"nonce too short", func(r *Request, _ *Mapping, _ *[]byte) { r.Nonce = "abc" }, "ATTESTATION_INVALID"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, m, b, s := fixture(t)
			c.edit(&r, &m, &b)
			att, err := Prepare(r, m, b, s, fixedNow, time.Minute, "lease-1")
			if err == nil {
				t.Fatalf("expected refusal, got %+v", att)
			}
			if att != nil {
				t.Fatalf("a refusal must return no attestation")
			}
			if got := codeOf(t, err); got != c.code {
				t.Fatalf("code %s want %s", got, c.code)
			}
		})
	}
}

func TestPrepare_RefusalEnvelopeLeaksNothing(t *testing.T) {
	r, m, _, s := fixture(t)
	r.BundleDigest = strings.Repeat("0", 64)
	b := []byte(`{"secret":"TOPSECRETBUNDLE"}`)
	_, err := Prepare(r, m, b, s, fixedNow, time.Minute, "lease-1")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "TOPSECRETBUNDLE") || strings.Contains(err.Error(), "TOPSECRETBUNDLE") {
		t.Fatalf("envelope leaks bundle bytes: %s", raw)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env["schema"] != ErrorSchema || env["phase"] != "prepare" || env["reviewer_started"] != false || env["code"] != "BUNDLE_INVALID" {
		t.Fatalf("envelope %s", raw)
	}
}

func TestVerify_Refusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Attestation, *Request, *time.Time)
	}{
		{"nonce differs", func(_ *Attestation, r *Request, _ *time.Time) { r.Nonce = "nonce-ffffffffffffffff" }},
		{"expired", func(_ *Attestation, _ *Request, n *time.Time) { *n = fixedNow.Add(time.Hour) }},
		{"image differs", func(_ *Attestation, r *Request, _ *time.Time) { r.Image = "reviewer@sha256:" + strings.Repeat("3", 64) }},
		{"bundle digest differs", func(_ *Attestation, r *Request, _ *time.Time) { r.BundleDigest = strings.Repeat("4", 64) }},
		{"binding digest differs", func(_ *Attestation, r *Request, _ *time.Time) { r.BindingDigest = strings.Repeat("5", 64) }},
		{"mapping differs", func(_ *Attestation, r *Request, _ *time.Time) { r.MappingID = "x" }},
		{"profile differs", func(_ *Attestation, r *Request, _ *time.Time) { r.Profile = "x" }},
		{"tampered entrypoint hash", func(a *Attestation, _ *Request, _ *time.Time) { a.EntrypointSHA256 = strings.Repeat("9", 64) }},
		{"tampered argv hash", func(a *Attestation, _ *Request, _ *time.Time) { a.ArgvSHA256 = strings.Repeat("9", 64) }},
		{"tampered lease", func(a *Attestation, _ *Request, _ *time.Time) { a.Lease = "lease-2" }},
		{"tampered expiry", func(a *Attestation, _ *Request, _ *time.Time) { a.ExpiresAt = a.ExpiresAt.Add(time.Hour) }},
		{"tampered policy digest", func(a *Attestation, _ *Request, _ *time.Time) { a.CapabilityPolicyDigest = strings.Repeat("9", 64) }},
		{"tampered egress policy", func(a *Attestation, _ *Request, _ *time.Time) { a.EgressPolicyID = "any" }},
		{"tampered credential ref", func(a *Attestation, _ *Request, _ *time.Time) { a.CredentialRef = "cred-9" }},
		{"enforced writes flipped", func(a *Attestation, _ *Request, _ *time.Time) { a.Enforced.FilesystemWrites = true }},
		{"enforced exec flipped", func(a *Attestation, _ *Request, _ *time.Time) { a.Enforced.AdditionalExec = true }},
		{"enforced host mounts flipped", func(a *Attestation, _ *Request, _ *time.Time) { a.Enforced.HostMounts = true }},
		{"enforced detached off", func(a *Attestation, _ *Request, _ *time.Time) { a.Enforced.DetachedGuest = false }},
		{"issuer swapped", func(a *Attestation, _ *Request, _ *time.Time) { a.Issuer = "someone-else" }},
		{"schema unsupported", func(a *Attestation, _ *Request, _ *time.Time) { a.Schema = "mgit.review-attest/v2" }},
		{"signature cleared", func(a *Attestation, _ *Request, _ *time.Time) { a.Signature = nil }},
		{"cwd changed", func(a *Attestation, _ *Request, _ *time.Time) { a.Cwd = "/" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, m, b, s := fixture(t)
			att, err := Prepare(r, m, b, s, fixedNow, 5*time.Minute, "lease-1")
			if err != nil {
				t.Fatal(err)
			}
			now := fixedNow.Add(time.Minute)
			c.edit(att, &r, &now)
			if got := codeOf(t, Verify(att, r, s.Public(), now)); got != "ATTESTATION_INVALID" {
				t.Fatalf("code %s", got)
			}
		})
	}
}

func TestVerify_WrongKeyRefused(t *testing.T) {
	r, m, b, s := fixture(t)
	att, err := Prepare(r, m, b, s, fixedNow, 5*time.Minute, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if got := codeOf(t, Verify(att, r, other, fixedNow)); got != "ATTESTATION_INVALID" {
		t.Fatalf("code %s", got)
	}
}

func TestVerify_JSONRoundTrip(t *testing.T) {
	r, m, b, s := fixture(t)
	att, err := Prepare(r, m, b, s, fixedNow, 5*time.Minute, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(att)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private", "TOPSECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("attestation carries %q", secret)
		}
	}
	var back Attestation
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := Verify(&back, r, s.Public(), fixedNow); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

func TestPolicyDigest_IsStableAndPinned(t *testing.T) {
	// The digest is what a client compares against; a change to the policy
	// must be a visible, deliberate change to this literal.
	const want = "3cc4b3c4338fab12dc13e9e92779471b62d03f3683a514aa913dedbfd562936a"
	if got := PolicyDigest(); got != want {
		t.Fatalf("policy digest %s, want %s", got, want)
	}
}

func TestSigner_RejectsBadKey(t *testing.T) {
	if _, err := NewSigner("x", ed25519.PrivateKey("short")); err == nil {
		t.Fatal("short key accepted")
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	if _, err := NewSigner("", priv); err == nil {
		t.Fatal("empty issuer accepted")
	}
}

// A correctly signed attestation that states a weaker policy than the
// profile is still refused: the signature proves who said it, not that it is
// acceptable.
func TestVerify_ValidlySignedButWeakerPolicyIsRefused(t *testing.T) {
	weaken := map[string]func(*Attestation){
		"writes":   func(a *Attestation) { a.Enforced.FilesystemWrites = true },
		"exec":     func(a *Attestation) { a.Enforced.AdditionalExec = true },
		"mounts":   func(a *Attestation) { a.Enforced.HostMounts = true },
		"detached": func(a *Attestation) { a.Enforced.DetachedGuest = false },
		"bundle":   func(a *Attestation) { a.Enforced.Bundle = "live" },
		"cwd":      func(a *Attestation) { a.Cwd = "/" },
		"policy":   func(a *Attestation) { a.CapabilityPolicyDigest = strings.Repeat("0", 64) },
	}
	for name, f := range weaken {
		t.Run(name, func(t *testing.T) {
			r, m, b, s := fixture(t)
			att, err := Prepare(r, m, b, s, fixedNow, 5*time.Minute, "lease-1")
			if err != nil {
				t.Fatal(err)
			}
			f(att)
			att.Signature = ed25519.Sign(s.priv, payload(att))
			if got := codeOf(t, Verify(att, r, s.Public(), fixedNow)); got != "ATTESTATION_INVALID" {
				t.Fatalf("code %s", got)
			}
		})
	}
}

func TestPrepare_NoArgvAttestsTheEmptyArray(t *testing.T) {
	r, m, b, s := fixture(t)
	m.Argv = nil
	att, err := Prepare(r, m, b, s, fixedNow, time.Minute, "lease-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := sum([]byte(`[]`)); att.ArgvSHA256 != want {
		t.Fatalf("argv digest %s want %s", att.ArgvSHA256, want)
	}
}

func TestPrepare_NonPositiveTTLIsRefused(t *testing.T) {
	r, m, b, s := fixture(t)
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := Prepare(r, m, b, s, fixedNow, ttl, "lease-1"); err == nil {
			t.Fatalf("ttl %v accepted", ttl)
		}
	}
}
