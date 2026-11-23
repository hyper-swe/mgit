package reviewguest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

const (
	evidenceName    = "evidence.json"
	attestationName = "attestation.json"
	maxAttestation  = 64 << 10
)

var leaseID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Lease is a sealed lease directory that has been checked against the
// attestation the daemon signed for it. Only OpenLease makes one, so a path
// that is not a sealed lease cannot reach Provision.
type Lease struct {
	dir   string
	image string
	att   reviewprep.Attestation
}

// Dir is the lease directory under the host's lease store.
func (l *Lease) Dir() string { return l.dir }

// Image is the digest-pinned image the attestation was issued for.
func (l *Lease) Image() string { return l.image }

// Attestation is a copy of the verified attestation.
func (l *Lease) Attestation() reviewprep.Attestation { return l.att }

func bad(detail string) error {
	return refuse(reviewprep.CodeBundleInvalid, detail)
}

// plain reports whether p is a regular file or directory (never a link) with
// exactly the given permission bits.
func plain(p string, dir bool, perm os.FileMode) bool {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() != dir {
		return false
	}
	if !dir && !fi.Mode().IsRegular() {
		return false
	}
	return fi.Mode().Perm() == perm
}

func readBounded(p string, max int64) ([]byte, bool) {
	f, err := os.Open(p) //nolint:gosec // host-owned lease path, checked by plain
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, false
	}
	return b, true
}

// OpenLease opens lease id under hostRoot/review/leases and checks that it is
// what the daemon sealed: a real directory (no links on the way) with mode
// 0700 that holds exactly evidence.json and attestation.json, each a regular
// read-only file; the attestation verifies against the host's public key for
// req and names this lease; and the evidence still hashes to what it signed.
func OpenLease(hostRoot, id string, req reviewprep.Request, pub ed25519.PublicKey, now time.Time) (*Lease, error) {
	if !filepath.IsAbs(hostRoot) || !leaseID.MatchString(id) {
		return nil, bad("the lease is not named by an absolute host root and a lease id")
	}
	store := filepath.Join(hostRoot, "review", "leases")
	dir := filepath.Join(store, id)
	if !plain(filepath.Join(hostRoot, "review"), true, 0o700) || !plain(store, true, 0o700) || !plain(dir, true, 0o700) {
		return nil, bad("the lease is not a private directory in the lease store")
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 2 {
		return nil, bad("the lease must hold exactly the evidence and its attestation")
	}
	evPath, atPath := filepath.Join(dir, evidenceName), filepath.Join(dir, attestationName)
	if !plain(evPath, false, 0o400) || !plain(atPath, false, 0o400) {
		return nil, bad("the lease files must be regular read-only files")
	}
	rawAtt, ok := readBounded(atPath, maxAttestation)
	if !ok {
		return nil, bad("the attestation is unreadable")
	}
	ev, ok := readBounded(evPath, reviewprep.MaxBundleBytes)
	if !ok {
		return nil, bad("the evidence is unreadable")
	}
	var att reviewprep.Attestation
	dec := json.NewDecoder(bytes.NewReader(rawAtt))
	dec.DisallowUnknownFields()
	invalid := func(d string) error {
		return &reviewprep.Error{Schema: reviewprep.ErrorSchema, Code: reviewprep.CodeAttestationInvalid, Phase: "prepare", Detail: d}
	}
	if err := dec.Decode(&att); err != nil {
		return nil, invalid("the attestation is not a valid record")
	}
	if err := reviewprep.Verify(&att, req, pub, now); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(ev)
	if att.Lease != id || att.BundleSHA256 != hex.EncodeToString(sum[:]) {
		return nil, invalid("the lease does not match its attestation")
	}
	return &Lease{dir: dir, image: req.Image, att: att}, nil
}
