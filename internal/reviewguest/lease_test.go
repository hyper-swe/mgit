package reviewguest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

const imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fixture seals a lease the way the daemon's review service does (a private
// directory under review/leases holding the evidence and the signed
// attestation, both read-only). The package cannot use the real service here
// because the service imports this package; the external test in
// lease_service_test.go opens a lease the real service sealed, which pins the
// two layouts together.
type fixture struct {
	root, id string
	pub      ed25519.PublicKey
	req      reviewprep.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := reviewprep.NewSigner("mgit-sandboxd", priv)
	if err != nil {
		t.Fatal(err)
	}
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigest,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"}, EgressPolicyID: "none",
	}
	ev := []byte(`{"citations":[{"n":1}]}`)
	h := sha256.Sum256(ev)
	req := reviewprep.Request{
		Profile: reviewprep.Profile, Image: "reviewer@" + imageDigest, MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDigest: hex.EncodeToString(h[:]), Nonce: "nonce-0123456789abcdef",
	}
	id := strings.Repeat("c", 32)
	att, err := reviewprep.Prepare(req, m, ev, sg, fixedNow, 5*time.Minute, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(att)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "review", "leases", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "review"), filepath.Join(root, "review", "leases"), dir} {
		if err := os.Chmod(d, 0o700); err != nil { //nolint:gosec // a lease directory is 0700, as the service makes it
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{"evidence.json": ev, "attestation.json": raw} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{root: root, id: id, pub: pub, req: req}
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
