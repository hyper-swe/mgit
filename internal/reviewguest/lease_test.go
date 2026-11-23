package reviewguest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

type noAudit struct{}

func (noAudit) RecordKeyChange(context.Context, string) error { return nil }

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

const imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fixture seals a real lease through the daemon's own review service, so what
// is opened here is exactly what production writes.
type fixture struct {
	root, id string
	pub      ed25519.PublicKey
	req      reviewprep.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	old := reviewprep.WriteDenialEnforced
	reviewprep.WriteDenialEnforced = true
	t.Cleanup(func() { reviewprep.WriteDenialEnforced = old })

	root := t.TempDir()
	if err := attest.GenerateKey(context.Background(), root, noAudit{}); err != nil {
		t.Fatal(err)
	}
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigest,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"}, EgressPolicyID: "none",
	}
	mp := filepath.Join(root, "review", "maps", "pi-v1.json")
	if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ev := []byte(`{"citations":[{"n":1}]}`)
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "evidence.json"), ev, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(ev)
	svc, err := reviewsvc.New(root, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatal(err)
	}
	args := controlproto.ReviewPrepareArgs{
		Profile: reviewprep.Profile, Image: "reviewer@" + imageDigest, MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDir: bundle, BundleDigest: hex.EncodeToString(h[:]), Nonce: "nonce-0123456789abcdef",
	}
	raw, err := svc.PrepareReview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var att reviewprep.Attestation
	if err := json.Unmarshal(raw, &att); err != nil {
		t.Fatal(err)
	}
	priv, err := attest.LoadSigningKey(root)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, id: att.Lease, pub: priv.Public().(ed25519.PublicKey), req: reviewprep.Request{
		Profile: args.Profile, Image: args.Image, MappingID: args.MappingID, BindingDigest: args.BindingDigest,
		BundleDigest: args.BundleDigest, Nonce: args.Nonce,
	}}
}

func (f *fixture) dir() string { return filepath.Join(f.root, "review", "leases", f.id) }

func (f *fixture) openAt(now time.Time) (*Lease, error) {
	return OpenLease(f.root, f.id, f.req, f.pub, now)
}

func (f *fixture) open(t *testing.T) *Lease {
	t.Helper()
	l, err := f.openAt(fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenLease_AcceptsTheLeaseTheServiceSealed(t *testing.T) {
	f := newFixture(t)
	l := f.open(t)
	if l.Dir() != f.dir() || l.Image() != f.req.Image || l.Attestation().Lease != f.id {
		t.Fatalf("dir=%q image=%q", l.Dir(), l.Image())
	}
}

func TestOpenLease_RefusesAnythingThatIsNotThatSealedLease(t *testing.T) {
	cases := []struct {
		name string
		do   func(t *testing.T, f *fixture) (*Lease, error)
		code string
	}{
		{"empty id", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease(f.root, "", f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"dotdot id", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease(f.root, "../"+f.id, f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"upper-case id", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease(f.root, strings.ToUpper(f.id), f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"short id", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease(f.root, f.id[:8], f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"relative host root", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease("review", f.id, f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"missing lease", func(t *testing.T, f *fixture) (*Lease, error) {
			return OpenLease(f.root, strings.Repeat("a", 32), f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"directory is group readable", func(t *testing.T, f *fixture) (*Lease, error) {
			mustDo(t, os.Chmod(f.dir(), 0o750)) //nolint:gosec // the test makes the directory too open on purpose
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"lease is a link", func(t *testing.T, f *fixture) (*Lease, error) {
			other := strings.Repeat("c", 32)
			mustDo(t, os.Symlink(f.dir(), filepath.Join(f.root, "review", "leases", other)))
			return OpenLease(f.root, other, f.req, f.pub, fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"lease store is a link", func(t *testing.T, f *fixture) (*Lease, error) {
			store := filepath.Join(f.root, "review", "leases")
			real := filepath.Join(f.root, "review", "real")
			mustDo(t, os.Rename(store, real))
			mustDo(t, os.Symlink(real, store))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"extra file", func(t *testing.T, f *fixture) (*Lease, error) {
			mustDo(t, os.WriteFile(filepath.Join(f.dir(), "extra"), nil, 0o400))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"evidence is writable", func(t *testing.T, f *fixture) (*Lease, error) {
			mustDo(t, os.Chmod(filepath.Join(f.dir(), "evidence.json"), 0o600))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"attestation is writable", func(t *testing.T, f *fixture) (*Lease, error) {
			mustDo(t, os.Chmod(filepath.Join(f.dir(), "attestation.json"), 0o600))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"attestation missing", func(t *testing.T, f *fixture) (*Lease, error) {
			mustDo(t, os.Remove(filepath.Join(f.dir(), "attestation.json")))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"attestation is a link", func(t *testing.T, f *fixture) (*Lease, error) {
			p := filepath.Join(f.dir(), "attestation.json")
			keep := filepath.Join(t.TempDir(), "a.json")
			raw, err := os.ReadFile(p) //nolint:gosec // test path
			mustDo(t, err)
			mustDo(t, os.WriteFile(keep, raw, 0o400)) //nolint:gosec // test path
			mustDo(t, os.Remove(p))
			mustDo(t, os.Symlink(keep, p))
			return f.openAt(fixedNow)
		}, reviewprep.CodeBundleInvalid},
		{"attestation is not json", func(t *testing.T, f *fixture) (*Lease, error) {
			p := filepath.Join(f.dir(), "attestation.json")
			mustDo(t, os.Chmod(p, 0o600))
			mustDo(t, os.WriteFile(p, []byte("not json"), 0o600))
			mustDo(t, os.Chmod(p, 0o400))
			return f.openAt(fixedNow)
		}, reviewprep.CodeAttestationInvalid},
		{"attestation carries a field it does not define", func(t *testing.T, f *fixture) (*Lease, error) {
			p := filepath.Join(f.dir(), "attestation.json")
			raw, err := os.ReadFile(p) //nolint:gosec // test path
			mustDo(t, err)
			var m map[string]any
			mustDo(t, json.Unmarshal(raw, &m))
			m["extra_field"] = "x" // not signed: only the strict decode catches it
			out, err := json.Marshal(m)
			mustDo(t, err)
			mustDo(t, os.Chmod(p, 0o600))
			mustDo(t, os.WriteFile(p, out, 0o600))
			mustDo(t, os.Chmod(p, 0o400))
			return f.openAt(fixedNow)
		}, reviewprep.CodeAttestationInvalid},
		{"evidence changed after sealing", func(t *testing.T, f *fixture) (*Lease, error) {
			p := filepath.Join(f.dir(), "evidence.json")
			mustDo(t, os.Chmod(p, 0o600))
			mustDo(t, os.WriteFile(p, []byte(`{"citations":[]}`), 0o600))
			mustDo(t, os.Chmod(p, 0o400))
			return f.openAt(fixedNow)
		}, reviewprep.CodeAttestationInvalid},
		{"signed by another key", func(t *testing.T, f *fixture) (*Lease, error) {
			pub, _, err := ed25519.GenerateKey(nil)
			mustDo(t, err)
			return OpenLease(f.root, f.id, f.req, pub, fixedNow)
		}, reviewprep.CodeAttestationInvalid},
		{"expired", func(t *testing.T, f *fixture) (*Lease, error) {
			return f.openAt(fixedNow.Add(48 * time.Hour))
		}, reviewprep.CodeAttestationInvalid},
		{"another request nonce", func(t *testing.T, f *fixture) (*Lease, error) {
			f.req.Nonce = "nonce-ffffffffffffffff"
			return f.openAt(fixedNow)
		}, reviewprep.CodeAttestationInvalid},
		{"sealed under another id", func(t *testing.T, f *fixture) (*Lease, error) {
			other := strings.Repeat("d", 32)
			dst := filepath.Join(f.root, "review", "leases", other)
			mustDo(t, os.Mkdir(dst, 0o700))
			for _, n := range []string{"evidence.json", "attestation.json"} {
				raw, err := os.ReadFile(filepath.Join(f.dir(), n)) //nolint:gosec // test path
				mustDo(t, err)
				mustDo(t, os.WriteFile(filepath.Join(dst, n), raw, 0o400)) //nolint:gosec // test path
			}
			return OpenLease(f.root, other, f.req, f.pub, fixedNow)
		}, reviewprep.CodeAttestationInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			l, err := c.do(t, f)
			if err == nil || l != nil || code(t, err) != c.code {
				t.Fatalf("lease=%v err=%v want %s", l, err, c.code)
			}
		})
	}
}
