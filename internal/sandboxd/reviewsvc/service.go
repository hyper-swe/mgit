// Package reviewsvc is the daemon-side service behind the review-prepare
// verb: it reads the operator-installed binding map and the detached evidence
// from the host, asks reviewprep to check and sign, and seals the evidence
// under a lease so what is later run is exactly what was attested. It starts
// no guest. Refs: MGIT-333
package reviewsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
)

const (
	issuer         = "mgit-sandboxd"
	defaultTTL     = 5 * time.Minute
	maxMapBytes    = 64 << 10
	evidenceName   = "evidence.json"
	attestationFil = "attestation.json"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Service issues review attestations from host state under hostRoot.
type Service struct {
	runner    *runner
	mapLoader MapLoader
	root      string
	signer    *reviewprep.Signer
	now       func() time.Time
	syncFile  func(*os.File) error // flushes a file or directory to stable storage; a seam for tests
}

// New loads the host signing key. A daemon without one cannot attest and
// fails closed.
func New(hostRoot string, now func() time.Time) (*Service, error) {
	if now == nil {
		return nil, fmt.Errorf("reviewsvc: clock must not be nil")
	}
	key, err := attest.LoadSigningKey(hostRoot)
	if err != nil {
		return nil, err
	}
	sg, err := reviewprep.NewSigner(issuer, key)
	if err != nil {
		return nil, err
	}
	return &Service{root: hostRoot, signer: sg, now: now, syncFile: (*os.File).Sync}, nil
}

// PrepareReview checks the request and returns the signed attestation JSON.
// A refusal is a *reviewprep.Error and leaves nothing behind.
func (s *Service) PrepareReview(_ context.Context, a controlproto.ReviewPrepareArgs) ([]byte, error) {
	if a.Profile != reviewprep.Profile {
		return nil, reviewprep.Refuse(reviewprep.CodeProfileUnavailable, "profile is not available")
	}
	m, err := s.loadMap(a.MappingID)
	if err != nil {
		return nil, err
	}
	evidence, err := readEvidence(a.BundleDir)
	if err != nil {
		return nil, err
	}
	lease, err := newLease()
	if err != nil {
		return nil, err
	}
	req := reviewprep.Request{Profile: a.Profile, Image: a.Image, MappingID: a.MappingID,
		BindingDigest: a.BindingDigest, BundleDigest: a.BundleDigest, Nonce: a.Nonce}
	att, err := reviewprep.Prepare(req, *m, evidence, s.signer, s.now(), defaultTTL, lease)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(att)
	if err != nil {
		return nil, fmt.Errorf("reviewsvc: encode attestation: %w", err)
	}
	if err := s.seal(lease, evidence, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// MapLoader reads the operator-installed mapping with the given id under
// hostRoot, or returns a refusal envelope. The id it receives has already been
// checked: letters, digits, dot, dash and underscore, starting with a letter or
// digit, at most 64 characters. It is how another package takes over
// map loading (checks on where the map came from, say) without editing this one.
type MapLoader func(hostRoot, id string) (*reviewprep.Mapping, error)

// WithMapLoader replaces how the service reads mappings and returns the service.
func (s *Service) WithMapLoader(l MapLoader) *Service {
	s.mapLoader = l
	return s
}

func (s *Service) loadMap(id string) (*reviewprep.Mapping, error) {
	// The id is checked here, ahead of any loader, so a replacement loader can
	// never be handed one that names a path.
	if !idPattern.MatchString(id) {
		return nil, reviewprep.Refuse(reviewprep.CodeMappingMismatch, "mapping is not installed")
	}
	if s.mapLoader != nil {
		return s.mapLoader(s.root, id)
	}
	return loadMapFromDisk(s.root, id)
}

// loadMapFromDisk is the default loader: hostRoot/review/maps/<id>.json, a
// regular file, strictly decoded.
func loadMapFromDisk(hostRoot, id string) (*reviewprep.Mapping, error) {
	bad := func(d string) error { return reviewprep.Refuse(reviewprep.CodeMappingMismatch, d) }
	if !idPattern.MatchString(id) {
		return nil, bad("mapping is not installed")
	}
	p := filepath.Join(hostRoot, "review", "maps", id+".json")
	raw, err := readRegular(p, maxMapBytes)
	if err != nil {
		return nil, bad("mapping is not installed")
	}
	var m reviewprep.Mapping
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, bad("mapping is not a valid map record")
	}
	return &m, nil
}

// readEvidence reads the detached bundle: a directory holding exactly one
// regular file, evidence.json. Anything else beside it, or a link, is refused,
// so no project instruction file rides along.
func readEvidence(dir string) ([]byte, error) {
	bad := func(d string) error { return reviewprep.Refuse(reviewprep.CodeBundleInvalid, d) }
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return nil, bad("bundle directory is not usable")
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || ents[0].Name() != evidenceName {
		return nil, bad("bundle directory must hold exactly evidence.json")
	}
	b, err := readRegular(filepath.Join(dir, evidenceName), reviewprep.MaxBundleBytes)
	if err != nil {
		return nil, bad("evidence is not a readable regular file")
	}
	return b, nil
}

// readRegular reads a regular file that is not a link, up to max bytes. The
// path is checked with Lstat and the opened file must be that same file, so a
// swap between the two is caught.
func readRegular(p string, max int64) ([]byte, error) {
	li, err := os.Lstat(p)
	if err != nil || !li.Mode().IsRegular() || li.Size() > max {
		return nil, fmt.Errorf("not a regular file within the size cap")
	}
	f, err := os.Open(p) //nolint:gosec // host-owned path, checked above
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(li, fi) || fi.Size() > max {
		return nil, fmt.Errorf("file changed while it was opened")
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

func newLease() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("reviewsvc: lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// seal writes the evidence and the attestation read-only under the lease. If
// any step fails, what was written is removed file by file.
func (s *Service) seal(lease string, evidence, attestation []byte) error {
	dir := filepath.Join(s.root, "review", "leases", lease)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("reviewsvc: lease store: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("reviewsvc: lease dir: %w", err)
	}
	var written []string
	undo := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
		_ = os.Remove(dir)
	}
	for name, data := range map[string][]byte{evidenceName: evidence, attestationFil: attestation} {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400) //nolint:gosec // host-owned lease path
		if err != nil {
			undo()
			return fmt.Errorf("reviewsvc: seal %s: %w", name, err)
		}
		written = append(written, p)
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			undo()
			return fmt.Errorf("reviewsvc: seal %s failed", name)
		}
	}
	return nil
}
