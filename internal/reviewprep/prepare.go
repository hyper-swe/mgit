// Package reviewprep is the pure half of `mgit sandbox review prepare`: it
// checks a review request against the operator-installed binding map and the
// bundle bytes, and issues (or verifies) a signed mgit.review-attest/v1.
//
// It touches no files, no network and no guest: the host key, the clock and
// the lease id are handed in. Every refusal happens before a reviewer could
// start, and says so (reviewer_started=false). Refs: MGIT-333
package reviewprep

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Schema and profile names.
const (
	Profile      = "reviewer-ro/v1"
	AttestSchema = "mgit.review-attest/v1"
	MapSchema    = "mgit.review-map/v1"
	ErrorSchema  = "mgit.review-error/v1"

	// Cwd is where the sealed bundle's directory is mounted in the guest.
	Cwd = "/review"

	minNonce = 16
)

// Refusal codes (contract item 4). All are pre-launch.
const (
	CodeProfileUnavailable = "PROFILE_UNAVAILABLE"
	CodeImageMismatch      = "IMAGE_MISMATCH"
	CodeMappingMismatch    = "MAPPING_MISMATCH"
	CodeBundleInvalid      = "BUNDLE_INVALID"
	CodePolicyUnenforcable = "POLICY_UNENFORCEABLE"
	CodeAttestationInvalid = "ATTESTATION_INVALID"
)

// Error is the refusal envelope. Detail is neutral: it never carries bundle
// bytes, prompts or credentials.
type Error struct {
	Schema          string `json:"schema"`
	Code            string `json:"code"`
	Phase           string `json:"phase"`
	ReviewerStarted bool   `json:"reviewer_started"`
	Detail          string `json:"detail"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

func refuse(code, detail string) *Error {
	return &Error{Schema: ErrorSchema, Code: code, Phase: "prepare", Detail: detail}
}

// Request is what the caller asks for.
type Request struct {
	Profile       string
	Image         string // name@sha256:<digest>
	MappingID     string
	BindingDigest string // hex sha256 of the host binding
	BundleDigest  string // hex sha256 of the exact evidence.json bytes
	Nonce         string
}

// Mapping is one operator-installed mgit.review-map/v1 record. It is never
// read from the bundle or from project files.
type Mapping struct {
	Schema           string   `json:"schema"`
	ID               string   `json:"id"`
	BindingSHA256    string   `json:"binding_sha256"`
	ImageDigest      string   `json:"image_digest"`
	GuestEntrypoint  string   `json:"guest_entrypoint"`
	EntrypointSHA256 string   `json:"entrypoint_sha256"`
	Argv             []string `json:"argv"`
	CredentialRef    string   `json:"credential_ref,omitempty"`
	EgressPolicyID   string   `json:"egress_policy_id,omitempty"`
}

// Enforced is the capability block the attestation states.
type Enforced struct {
	DetachedGuest    bool   `json:"detached_guest"`
	Bundle           string `json:"bundle"`
	FilesystemWrites bool   `json:"filesystem_writes"`
	AdditionalExec   bool   `json:"additional_exec"`
	HostMounts       bool   `json:"host_mounts"`
}

// Attestation is mgit.review-attest/v1. It holds identities and policy ids
// only, never a secret value.
type Attestation struct {
	Schema                 string    `json:"schema"`
	Issuer                 string    `json:"issuer"`
	Nonce                  string    `json:"nonce"`
	Lease                  string    `json:"lease"`
	ExpiresAt              time.Time `json:"expires_at"`
	Profile                string    `json:"profile"`
	ImageDigest            string    `json:"image_digest"`
	MappingID              string    `json:"mapping_id"`
	BindingSHA256          string    `json:"binding_sha256"`
	EntrypointSHA256       string    `json:"entrypoint_sha256"`
	ArgvSHA256             string    `json:"argv_sha256"`
	BundleSHA256           string    `json:"bundle_sha256"`
	Cwd                    string    `json:"cwd"`
	CapabilityPolicyDigest string    `json:"capability_policy_digest"`
	Enforced               Enforced  `json:"enforced"`
	EgressPolicyID         string    `json:"egress_policy_id,omitempty"`
	CredentialRef          string    `json:"credential_ref,omitempty"`
	Signature              []byte    `json:"signature"`
}

// policyText names every restriction the profile enforces. Its digest rides
// in the attestation, so a change to the policy is a visible change.
const policyText = "reviewer-ro/v1|detached-guest|bundle=sealed-ro|fs-writes=false|additional-exec=false|host-mounts=false|no_new_privs|empty-caps|exec-by-fd-once|cwd=/review"

// PolicyDigest is the hex sha256 of the profile's policy text.
func PolicyDigest() string { return hexSum([]byte(policyText)) }

func hexSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Signer signs attestations with the host key.
type Signer struct {
	issuer string
	priv   ed25519.PrivateKey
}

// NewSigner wraps a host Ed25519 key. An empty issuer or a malformed key is
// refused (fail closed).
func NewSigner(issuer string, priv ed25519.PrivateKey) (*Signer, error) {
	if issuer == "" {
		return nil, fmt.Errorf("reviewprep: issuer must not be empty")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("reviewprep: signing key is not an Ed25519 private key")
	}
	return &Signer{issuer: issuer, priv: priv}, nil
}

// Public returns the verification key.
func (s *Signer) Public() ed25519.PublicKey {
	pub, _ := s.priv.Public().(ed25519.PublicKey)
	return pub
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

// digestOf returns the digest part of name@sha256:<hex>, or "".
func digestOf(image string) string {
	i := strings.LastIndex(image, "@")
	if i < 0 {
		return ""
	}
	d := image[i+1:]
	if !strings.HasPrefix(d, "sha256:") || !isHex64(strings.TrimPrefix(d, "sha256:")) {
		return ""
	}
	return d
}

func argvDigest(argv []string) (string, error) {
	if argv == nil {
		argv = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(argv); err != nil {
		return "", err
	}
	return hexSum(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// Prepare checks the request against the map and the bundle and returns a
// signed attestation. Any failure returns a nil attestation and a *Error
// with reviewer_started=false.
func Prepare(r Request, m Mapping, bundle []byte, s *Signer, now time.Time, ttl time.Duration, lease string) (*Attestation, error) {
	if r.Profile != Profile {
		return nil, refuse(CodeProfileUnavailable, "profile is not available")
	}
	if m.Schema != MapSchema || m.ID != r.MappingID || r.MappingID == "" {
		return nil, refuse(CodeMappingMismatch, "mapping does not match the request")
	}
	if !isHex64(r.BindingDigest) || m.BindingSHA256 != r.BindingDigest {
		return nil, refuse(CodeMappingMismatch, "binding digest does not match the mapping")
	}
	want := digestOf(r.Image)
	if want == "" || want != m.ImageDigest {
		return nil, refuse(CodeImageMismatch, "image digest does not match the mapping")
	}
	if len(bundle) == 0 || !json.Valid(bundle) || !isHex64(r.BundleDigest) || hexSum(bundle) != r.BundleDigest {
		return nil, refuse(CodeBundleInvalid, "bundle is not the declared evidence")
	}
	if !path.IsAbs(m.GuestEntrypoint) || path.Clean(m.GuestEntrypoint) != m.GuestEntrypoint || !isHex64(m.EntrypointSHA256) {
		return nil, refuse(CodePolicyUnenforcable, "mapped entrypoint is not a pinned absolute path")
	}
	if m.CredentialRef != "" && m.EgressPolicyID == "" {
		return nil, refuse(CodePolicyUnenforcable, "a credential needs a declared egress policy")
	}
	if len(r.Nonce) < minNonce {
		return nil, refuse(CodeAttestationInvalid, "nonce is missing or too short")
	}
	if s == nil || lease == "" || ttl <= 0 {
		return nil, refuse(CodeAttestationInvalid, "attestation cannot be issued")
	}
	ad, err := argvDigest(m.Argv)
	if err != nil {
		return nil, refuse(CodeMappingMismatch, "argv cannot be encoded")
	}
	a := &Attestation{
		Schema:                 AttestSchema,
		Issuer:                 s.issuer,
		Nonce:                  r.Nonce,
		Lease:                  lease,
		ExpiresAt:              now.UTC().Add(ttl),
		Profile:                Profile,
		ImageDigest:            m.ImageDigest,
		MappingID:              m.ID,
		BindingSHA256:          m.BindingSHA256,
		EntrypointSHA256:       m.EntrypointSHA256,
		ArgvSHA256:             ad,
		BundleSHA256:           r.BundleDigest,
		Cwd:                    Cwd,
		CapabilityPolicyDigest: PolicyDigest(),
		Enforced:               Enforced{DetachedGuest: true, Bundle: "sealed-ro"},
		EgressPolicyID:         m.EgressPolicyID,
		CredentialRef:          m.CredentialRef,
	}
	a.Signature = ed25519.Sign(s.priv, payload(a))
	return a, nil
}

// Verify is the client side: every requested identity, the signature, the
// issuer's key and the expiry must agree, or the reviewer must not run.
func Verify(a *Attestation, r Request, pub ed25519.PublicKey, now time.Time) error {
	bad := func(d string) error {
		return &Error{Schema: ErrorSchema, Code: CodeAttestationInvalid, Phase: "verify", Detail: d}
	}
	if a == nil || a.Schema != AttestSchema {
		return bad("unsupported attestation")
	}
	if len(pub) != ed25519.PublicKeySize || len(a.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(pub, payload(a), a.Signature) {
		return bad("signature does not verify")
	}
	if !now.Before(a.ExpiresAt) {
		return bad("attestation expired")
	}
	if a.Nonce != r.Nonce || a.Profile != r.Profile || a.MappingID != r.MappingID ||
		a.BindingSHA256 != r.BindingDigest || a.BundleSHA256 != r.BundleDigest ||
		a.ImageDigest != digestOf(r.Image) || a.ImageDigest == "" {
		return bad("attestation does not match the request")
	}
	e := a.Enforced
	if !e.DetachedGuest || e.Bundle != "sealed-ro" || e.FilesystemWrites || e.AdditionalExec || e.HostMounts {
		return bad("enforced policy is weaker than the profile")
	}
	if a.Cwd != Cwd || a.CapabilityPolicyDigest != PolicyDigest() {
		return bad("policy does not match the profile")
	}
	return nil
}

// payload is the exact byte string that is signed: a domain tag, then every
// attested field length-prefixed in a fixed order, so no field boundary can
// be shifted and no field can be dropped without changing the bytes.
func payload(a *Attestation) []byte {
	var b bytes.Buffer
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		b.Write(n[:])
		b.WriteString(s)
	}
	flag := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	put(AttestSchema)
	for _, f := range []string{
		a.Schema, a.Issuer, a.Nonce, a.Lease, a.ExpiresAt.UTC().Format(time.RFC3339Nano),
		a.Profile, a.ImageDigest, a.MappingID, a.BindingSHA256, a.EntrypointSHA256,
		a.ArgvSHA256, a.BundleSHA256, a.Cwd, a.CapabilityPolicyDigest,
		flag(a.Enforced.DetachedGuest), a.Enforced.Bundle, flag(a.Enforced.FilesystemWrites),
		flag(a.Enforced.AdditionalExec), flag(a.Enforced.HostMounts),
		a.EgressPolicyID, a.CredentialRef,
	} {
		put(f)
	}
	return b.Bytes()
}
