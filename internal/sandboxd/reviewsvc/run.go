package reviewsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewguest"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// RunnerConfig is what the run verb needs to start a review guest. Without it
// the service can prepare but not run.
type RunnerConfig struct {
	// Manager launches and removes guests; Backend names the kind it reports.
	// Only a launch-time-image backend is accepted.
	Manager model.SandboxManager
	Backend string
	// ReviewerUID and ReviewerGID are the unprivileged identity the reviewer
	// runs as; zero means 65534 (nobody).
	ReviewerUID, ReviewerGID int
	// Bindings installs the binding map, network policy and credential; nil
	// means NoBindings, which grants nothing.
	Bindings Bindings
}

type runner struct{ RunnerConfig }

const defaultReviewerID = 65534

// WithRunner enables the run verb and returns the service.
func (s *Service) WithRunner(c RunnerConfig) *Service {
	if c.Manager == nil {
		s.runner = nil
		return s
	}
	if c.ReviewerUID <= 0 {
		c.ReviewerUID = defaultReviewerID
	}
	if c.ReviewerGID <= 0 {
		c.ReviewerGID = defaultReviewerID
	}
	if c.Bindings == nil {
		c.Bindings = NoBindings{}
	}
	s.runner = &runner{c}
	return s
}

func started(code, detail string) *reviewprep.Error {
	e := reviewprep.Refuse(code, detail)
	e.Phase = "run"
	e.ReviewerStarted = true
	return e
}

func asRefusal(err error, detail string) *reviewprep.Error {
	var e *reviewprep.Error
	if errors.As(err, &e) {
		return e
	}
	return reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, detail)
}

// RunReview runs one prepared review and returns the result JSON, or a
// *reviewprep.Error. It re-verifies the lease and the mapping, consumes the
// nonce, provisions and seals the guest, installs the bindings, runs the mapped
// program under the limits, and removes the guest whatever happened.
func (s *Service) RunReview(ctx context.Context, a controlproto.ReviewRunArgs) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // the caller is already gone: nothing is consumed or started
	}
	if a.Profile != reviewprep.Profile {
		return nil, reviewprep.Refuse(reviewprep.CodeProfileUnavailable, "profile is not available")
	}
	rn := s.runner
	if rn == nil {
		return nil, reviewprep.Refuse(reviewprep.CodeProfileUnavailable, "this daemon has no review guest backend")
	}
	if rn.Backend != model.BackendKVM {
		return nil, reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "this backend does not deliver the bundle as a launch-time image")
	}
	m, err := s.loadMap(a.MappingID)
	if err != nil {
		return nil, err
	}
	req := reviewprep.Request{Profile: a.Profile, Image: a.Image, MappingID: a.MappingID,
		BindingDigest: a.BindingDigest, BundleDigest: a.BundleDigest, Nonce: a.Nonce}
	lease, err := reviewguest.OpenLease(s.root, a.Lease, req, s.signer.Public(), s.now())
	if err != nil {
		return nil, asRefusal(err, "the lease could not be verified")
	}
	att := lease.Attestation()
	if err := reviewprep.CheckMapping(&att, *m); err != nil {
		return nil, asRefusal(err, "the mapping is not the attested one")
	}
	secs, maxBytes := a.MaxSeconds, a.MaxOutputBytes
	if secs == 0 {
		secs = controlproto.DefaultReviewSeconds
	}
	if maxBytes == 0 {
		maxBytes = controlproto.DefaultReviewOutputBytes
	}
	// The last point at which the caller can go away without spending the lease.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.consume(a.Lease, a.Nonce); err != nil {
		return nil, err
	}

	g, err := reviewguest.Provision(ctx, rn.Manager, reviewguest.Spec{
		Backend: rn.Backend, Lease: lease, TaskID: "REVIEW-" + a.Lease[:16],
		UID: rn.ReviewerUID, GID: rn.ReviewerGID,
	})
	if err != nil {
		return nil, asRefusal(err, "the review guest could not be provisioned")
	}
	defer func() { _ = g.Close(context.WithoutCancel(ctx)) }()

	release, err := rn.Bindings.Attach(ctx, AttachInput{
		GuestID: g.ID, LeaseDir: lease.Dir(), Attestation: att, Mapping: *m,
		Exec: func(c context.Context, r model.ExecRequest) (*model.ExecResult, error) {
			return rn.Manager.Exec(c, g.ID, r)
		},
	})
	// The release is registered before the error is looked at: the Bindings
	// contract hands one back on every path, a refusal included, so whatever
	// Attach installed before it refused is taken down too.
	if release != nil {
		defer func() { _ = release(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		return nil, asRefusal(err, "the review's bindings could not be installed")
	}

	began := s.now()
	res, err := g.Run(ctx, reviewguest.Entry{
		SHA256: m.EntrypointSHA256, Path: m.GuestEntrypoint, Argv: m.Argv,
		Timeout: time.Duration(secs) * time.Second,
	})
	elapsed := s.now().Sub(began)
	if err != nil {
		var refusal *reviewprep.Error
		if errors.As(err, &refusal) {
			return nil, refusal // the launcher declined: the reviewer did not start
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, started(reviewprep.CodeRunTimeout, "the review did not finish within its time limit")
		}
		return nil, started(reviewprep.CodePolicyUnenforcable, "the review could not be run to its end")
	}
	if int64(len(res.Stdout))+int64(len(res.Stderr)) > maxBytes {
		return nil, started(reviewprep.CodeOutputLimit, "the review produced more output than its limit")
	}
	out, err := json.Marshal(controlproto.ReviewResult{
		Schema: controlproto.ReviewResultSchema, Lease: a.Lease, ExitCode: res.ExitCode,
		Stdout: res.Stdout, Stderr: res.Stderr, ElapsedMS: elapsed.Milliseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("reviewsvc: encode result: %w", err)
	}
	return out, nil
}

// consume records that a lease has been run, refusing a second run of it. The
// record is created exclusively, so two racing runs cannot both pass.
func (s *Service) consume(lease, nonce string) error {
	dir := filepath.Join(s.root, "review", "used")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "the run could not be recorded")
	}
	sum := sha256.Sum256([]byte(lease))
	p := filepath.Join(dir, hex.EncodeToString(sum[:]))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // host-owned record path from a digest
	if errors.Is(err, os.ErrExist) {
		return reviewprep.Refuse(reviewprep.CodeNonceReplayed, "this lease has already been run")
	}
	if err != nil {
		return reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "the run could not be recorded")
	}
	nsum := sha256.Sum256([]byte(nonce))
	_, werr := f.WriteString(hex.EncodeToString(nsum[:]) + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "the run could not be recorded")
	}
	return nil
}
