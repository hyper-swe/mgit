package reviewguest_test

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
	"github.com/hyper-swe/mgit/internal/reviewguest"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

type noAudit struct{}

func (noAudit) RecordKeyChange(context.Context, string) error { return nil }

// A lease the daemon's own review service sealed must open: this ties the layout
// OpenLease expects to the layout the service writes, which the package's own
// fixture can only imitate (the service imports this package).
func TestOpenLease_OpensALeaseTheReviewServiceSealed(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if err := attest.GenerateKey(context.Background(), root, noAudit{}); err != nil {
		t.Fatal(err)
	}
	const image = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"}, EgressPolicyID: "none",
	}
	mp := filepath.Join(root, "review", "maps", "pi-v1.json")
	if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(mp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ev := []byte(`{"citations":[{"n":1}]}`)
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "evidence.json"), ev, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(ev)
	svc, err := reviewsvc.New(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	args := controlproto.ReviewPrepareArgs{
		Profile: reviewprep.Profile, Image: "reviewer@" + image, MappingID: "pi-v1", BindingDigest: strings.Repeat("b", 64),
		BundleDir: bundle, BundleDigest: hex.EncodeToString(h[:]), Nonce: "nonce-0123456789abcdef",
	}
	attRaw, err := svc.PrepareReview(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var att reviewprep.Attestation
	if err := json.Unmarshal(attRaw, &att); err != nil {
		t.Fatal(err)
	}
	priv, err := attest.LoadSigningKey(root)
	if err != nil {
		t.Fatal(err)
	}
	req := reviewprep.Request{Profile: args.Profile, Image: args.Image, MappingID: args.MappingID,
		BindingDigest: args.BindingDigest, BundleDigest: args.BundleDigest, Nonce: args.Nonce}
	l, err := reviewguest.OpenLease(root, att.Lease, req, priv.Public().(ed25519.PublicKey), now.Add(time.Second))
	if err != nil {
		t.Fatalf("a lease the service sealed did not open: %v", err)
	}
	if l.Attestation().Lease != att.Lease {
		t.Fatalf("lease %q", l.Attestation().Lease)
	}
}
