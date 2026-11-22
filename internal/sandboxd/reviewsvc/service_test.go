package reviewsvc

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
)

type noAudit struct{}

func (noAudit) RecordKeyChange(context.Context, string) error { return nil }

var now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

const image = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

type env struct {
	root, bundle string
	svc          *Service
	args         controlproto.ReviewPrepareArgs
	evidence     []byte
}

func setup(t *testing.T) *env {
	t.Helper()
	old := reviewprep.WriteDenialEnforced
	reviewprep.WriteDenialEnforced = true
	t.Cleanup(func() { reviewprep.WriteDenialEnforced = old })
	root := t.TempDir()
	if err := attest.GenerateKey(context.Background(), root, noAudit{}); err != nil {
		t.Fatal(err)
	}
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"}, EgressPolicyID: "none",
	}
	writeJSON(t, filepath.Join(root, "review", "maps", "pi-v1.json"), m)
	ev := []byte(`{"citations":[{"n":1}]}`)
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "evidence.json"), ev, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(ev)
	svc, err := New(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return &env{root: root, bundle: bundle, svc: svc, evidence: ev, args: controlproto.ReviewPrepareArgs{
		Profile: reviewprep.Profile, Image: "reviewer@" + image, MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDir: bundle, BundleDigest: hex.EncodeToString(h[:]), Nonce: "nonce-0123456789abcdef"}}
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatalf("want a refusal envelope, got %v", err)
	}
	return e.Code
}

func leases(t *testing.T, root string) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(root, "review", "leases"))
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPrepareReview_IssuesAVerifiableAttestationAndSealsTheBundle(t *testing.T) {
	e := setup(t)
	raw, err := e.svc.PrepareReview(context.Background(), e.args)
	if err != nil {
		t.Fatal(err)
	}
	var att reviewprep.Attestation
	if err := json.Unmarshal(raw, &att); err != nil {
		t.Fatal(err)
	}
	priv, err := attest.LoadSigningKey(e.root)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	req := reviewprep.Request{Profile: e.args.Profile, Image: e.args.Image, MappingID: e.args.MappingID,
		BindingDigest: e.args.BindingDigest, BundleDigest: e.args.BundleDigest, Nonce: e.args.Nonce}
	if err := reviewprep.Verify(&att, req, pub, now.Add(time.Second)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if att.Lease == "" || len(att.Lease) < 16 {
		t.Fatalf("lease %q", att.Lease)
	}
	sealed := filepath.Join(e.root, "review", "leases", att.Lease, "evidence.json")
	got, err := os.ReadFile(sealed) //nolint:gosec // test-owned temp path
	if err != nil || string(got) != string(e.evidence) {
		t.Fatalf("sealed bundle %q err=%v", got, err)
	}
	fi, _ := os.Stat(sealed)
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("sealed bundle mode %v, want 0400", fi.Mode().Perm())
	}
	attFile := filepath.Join(e.root, "review", "leases", att.Lease, "attestation.json")
	sealedAtt, err := os.ReadFile(attFile) //nolint:gosec // test-owned temp path
	if err != nil || string(sealedAtt) != string(raw) {
		t.Fatalf("the lease must hold the attestation that was returned: err=%v", err)
	}
	if fi, _ := os.Stat(attFile); fi.Mode().Perm() != 0o400 {
		t.Fatalf("sealed attestation mode %v, want 0400", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(sealed)); !di.IsDir() || di.Mode().Perm() != 0o700 {
		t.Fatalf("lease directory mode %v, want 0700", di.Mode().Perm())
	}
	if si, _ := os.Stat(filepath.Join(e.root, "review", "leases")); si.Mode().Perm() != 0o700 {
		t.Fatalf("lease store mode %v, want 0700", si.Mode().Perm())
	}
	// The sealed copy is what runs; changing the source after prepare changes nothing.
	if err := os.WriteFile(filepath.Join(e.bundle, "evidence.json"), []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(sealed) //nolint:gosec // test-owned temp path
	if string(again) != string(e.evidence) {
		t.Fatal("the sealed copy followed the source")
	}
}

func TestPrepareReview_LeasesAreDistinct(t *testing.T) {
	e := setup(t)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		raw, err := e.svc.PrepareReview(context.Background(), e.args)
		if err != nil {
			t.Fatal(err)
		}
		var a reviewprep.Attestation
		_ = json.Unmarshal(raw, &a)
		if seen[a.Lease] {
			t.Fatalf("lease %s repeated", a.Lease)
		}
		seen[a.Lease] = true
	}
}

func TestPrepareReview_Refusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*env)
		code string
	}{
		{"no such mapping", func(e *env) { e.args.MappingID = "nope" }, "MAPPING_MISMATCH"},
		{"map has an unknown field", func(e *env) {
			p := filepath.Join(e.root, "review", "maps", "pi-v1.json")
			b, _ := os.ReadFile(p) //nolint:gosec // test-owned temp path
			b = []byte(strings.Replace(string(b), `{`, `{"trust_all":true,`, 1))
			_ = os.WriteFile(p, b, 0o600) //nolint:gosec // test-owned temp path
		}, "MAPPING_MISMATCH"},
		{"map is a symlink", func(e *env) {
			p := filepath.Join(e.root, "review", "maps", "pi-v1.json")
			_ = os.Rename(p, p+".real")
			_ = os.Symlink(p+".real", p)
		}, "MAPPING_MISMATCH"},
		{"image differs", func(e *env) { e.args.Image = "reviewer@sha256:" + strings.Repeat("2", 64) }, "IMAGE_MISMATCH"},
		{"bundle digest differs", func(e *env) { e.args.BundleDigest = strings.Repeat("0", 64) }, "BUNDLE_INVALID"},
		{"bundle dir missing", func(e *env) { e.args.BundleDir = filepath.Join(e.bundle, "gone") }, "BUNDLE_INVALID"},
		{"evidence is a symlink", func(e *env) {
			p := filepath.Join(e.bundle, "evidence.json")
			_ = os.Rename(p, filepath.Join(e.bundle, "real.bin"))
			_ = os.Symlink(filepath.Join(e.bundle, "real.bin"), p)
		}, "BUNDLE_INVALID"},
		{"extra file beside the evidence", func(e *env) {
			_ = os.WriteFile(filepath.Join(e.bundle, "AGENTS.md"), []byte("be nice"), 0o600)
		}, "BUNDLE_INVALID"},
		{"bundle directory is a symlink", func(e *env) {
			link := filepath.Join(t.TempDir(), "link")
			_ = os.Symlink(e.bundle, link)
			e.args.BundleDir = link
		}, "BUNDLE_INVALID"},
		{"extra file sorting after the evidence", func(e *env) {
			_ = os.WriteFile(filepath.Join(e.bundle, "zzz-instructions.md"), []byte("be nice"), 0o600)
		}, "BUNDLE_INVALID"},
		{"profile unknown", func(e *env) { e.args.Profile = "reviewer-rw/v1" }, "PROFILE_UNAVAILABLE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			c.edit(e)
			raw, err := e.svc.PrepareReview(context.Background(), e.args)
			if raw != nil {
				t.Fatalf("a refusal returned an attestation: %s", raw)
			}
			if got := code(t, err); got != c.code {
				t.Fatalf("code %s want %s", got, c.code)
			}
			if l := leases(t, e.root); len(l) != 0 {
				t.Fatalf("a refusal left a lease behind: %v", l)
			}
		})
	}
}

func TestNew_FailsClosedWithoutAKey(t *testing.T) {
	if _, err := New(t.TempDir(), func() time.Time { return now }); err == nil {
		t.Fatal("a service without the host key was created")
	}
}

// The mapping id names a file under the map directory and nothing else. An id
// that walks out of it would let a caller-written record be attested, so the
// service refuses it whatever the protocol layer checked first.
func TestPrepareReview_MappingIDCannotLeaveTheMapDirectory(t *testing.T) {
	for _, id := range []string{"../other/pi-v1", "other/../pi-v1", "a/b", ".hidden", "..", "", "pi-v1/", "x\x00y"} {
		t.Run(id, func(t *testing.T) {
			e := setup(t)
			m := reviewprep.Mapping{
				Schema: reviewprep.MapSchema, ID: id, BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
				GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), EgressPolicyID: "none",
			}
			writeJSON(t, filepath.Join(e.root, "review", "other", "pi-v1.json"), m)
			e.args.MappingID = id
			raw, err := e.svc.PrepareReview(context.Background(), e.args)
			if raw != nil {
				t.Fatalf("a caller-written map was attested: %s", raw)
			}
			if got := code(t, err); got != "MAPPING_MISMATCH" {
				t.Fatalf("code %s", got)
			}
			if l := leases(t, e.root); len(l) != 0 {
				t.Fatalf("lease left behind: %v", l)
			}
		})
	}
}
