package reviewsvc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// runMgr is a manager that records what the run asked of it. Execs carrying an
// environment are the reviewer's run (the nonce travels there); the rest are the
// seal step.
type runMgr struct {
	mu       sync.Mutex
	backend  string
	launched []model.SandboxLaunchOptions
	execs    []model.ExecRequest
	removed  []string
	runRes   func(model.ExecRequest) (*model.ExecResult, error)
	sealRes  *model.ExecResult
}

func (m *runMgr) Launch(_ context.Context, o model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launched = append(m.launched, o)
	return &model.SandboxInfo{ID: "sb-run", Backend: m.backend, WorktreePath: o.WorktreePath}, nil
}
func (m *runMgr) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (m *runMgr) Exec(_ context.Context, _ string, r model.ExecRequest) (*model.ExecResult, error) {
	m.mu.Lock()
	m.execs = append(m.execs, r)
	m.mu.Unlock()
	if len(r.Env) == 0 {
		if m.sealRes != nil {
			return m.sealRes, nil
		}
		return &model.ExecResult{}, nil
	}
	if m.runRes != nil {
		return m.runRes(r)
	}
	return &model.ExecResult{Stdout: []byte("verdict")}, nil
}
func (m *runMgr) Stop(context.Context, string, bool) error { return nil }
func (m *runMgr) Remove(_ context.Context, id string, _ bool) error {
	m.mu.Lock()
	m.removed = append(m.removed, id)
	m.mu.Unlock()
	return nil
}
func (m *runMgr) Resolve(context.Context, string) (*model.SandboxInfo, error) { return nil, nil }

// recBindings records the order of what the run does around the seam.
type recBindings struct {
	err      error
	attached []AttachInput
	released int
}

func (b *recBindings) Attach(_ context.Context, in AttachInput) (func(context.Context) error, error) {
	b.attached = append(b.attached, in)
	if b.err != nil {
		return nil, b.err
	}
	return func(context.Context) error { b.released++; return nil }, nil
}

type runEnv struct {
	*env
	mgr  *runMgr
	args controlproto.ReviewRunArgs
}

// prepared prepares a lease through the real service and returns run arguments
// for it, with a fake manager behind the runner.
func prepared(t *testing.T) *runEnv {
	t.Helper()
	e := setup(t)
	raw, err := e.svc.PrepareReview(context.Background(), e.args)
	if err != nil {
		t.Fatal(err)
	}
	var att reviewprep.Attestation
	if err := json.Unmarshal(raw, &att); err != nil {
		t.Fatal(err)
	}
	m := &runMgr{backend: model.BackendKVM}
	e.svc.WithRunner(RunnerConfig{Manager: m, Backend: model.BackendKVM})
	return &runEnv{env: e, mgr: m, args: controlproto.ReviewRunArgs{
		Profile: e.args.Profile, Image: e.args.Image, MappingID: e.args.MappingID, BindingDigest: e.args.BindingDigest,
		BundleDigest: e.args.BundleDigest, Nonce: e.args.Nonce, Lease: att.Lease,
	}}
}

func (r *runEnv) run(t *testing.T) (*controlproto.ReviewResult, error) {
	t.Helper()
	raw, err := r.svc.RunReview(context.Background(), r.args)
	if err != nil {
		return nil, err
	}
	var res controlproto.ReviewResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return &res, nil
}

func envelopeOf(t *testing.T, err error) *reviewprep.Error {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatalf("want a refusal envelope, got %v", err)
	}
	return e
}

func usedRecords(t *testing.T, root string) int {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(root, "review", "used"))
	return len(ents)
}

func TestRunReview_RunsTheMappedProgramAndReturnsWhatItProduced(t *testing.T) {
	r := prepared(t)
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Schema != controlproto.ReviewResultSchema || res.Lease != r.args.Lease || res.ExitCode != 0 || string(res.Stdout) != "verdict" {
		t.Fatalf("result %+v", res)
	}
	if len(r.mgr.launched) != 1 || r.mgr.launched[0].ImageRef != r.args.Image {
		t.Fatalf("launch %+v", r.mgr.launched)
	}
	// What ran is what the operator's mapping says, never anything of the caller's.
	run := r.mgr.execs[len(r.mgr.execs)-1].Command
	joined := strings.Join(run, " ")
	for _, want := range []string{"review-exec", "--sha256 " + strings.Repeat("e", 64), "-- /opt/r/bin/r --json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command %q lacks %q", joined, want)
		}
	}
	if len(r.mgr.removed) != 1 {
		t.Fatalf("the guest was not removed: %v", r.mgr.removed)
	}
	if usedRecords(t, r.root) != 1 {
		t.Fatal("the run left no record that the lease was used")
	}
}

func TestRunReview_TheReviewersOwnExitCodeIsReturnedAsData(t *testing.T) {
	r := prepared(t)
	r.mgr.runRes = func(model.ExecRequest) (*model.ExecResult, error) {
		return &model.ExecResult{ExitCode: 3, Stdout: []byte("o"), Stderr: []byte("e")}, nil
	}
	res, err := r.run(t)
	if err != nil || res.ExitCode != 3 || string(res.Stderr) != "e" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRunReview_ASecondRunOfTheSameLeaseIsRefusedBeforeAnythingStarts(t *testing.T) {
	r := prepared(t)
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
	launches := len(r.mgr.launched)
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodeNonceReplayed || e.ReviewerStarted {
		t.Fatalf("envelope %+v", e)
	}
	if len(r.mgr.launched) != launches {
		t.Fatal("a replay launched a guest")
	}
}

func TestRunReview_ARefusalBeforeTheNonceIsConsumedDoesNotConsumeIt(t *testing.T) {
	cases := map[string]struct {
		edit func(*runEnv)
		code string
	}{
		"unknown lease":    {func(r *runEnv) { r.args.Lease = strings.Repeat("a", 32) }, reviewprep.CodeBundleInvalid},
		"another nonce":    {func(r *runEnv) { r.args.Nonce = "nonce-ffffffffffffffff" }, reviewprep.CodeAttestationInvalid},
		"another binding":  {func(r *runEnv) { r.args.BindingDigest = strings.Repeat("c", 64) }, reviewprep.CodeAttestationInvalid},
		"another profile":  {func(r *runEnv) { r.args.Profile = "other/v1" }, reviewprep.CodeProfileUnavailable},
		"unknown mapping":  {func(r *runEnv) { r.args.MappingID = "nope" }, reviewprep.CodeMappingMismatch},
		"unattested image": {func(r *runEnv) { r.args.Image = "reviewer@sha256:" + strings.Repeat("2", 64) }, reviewprep.CodeAttestationInvalid},
	}
	for name, c := range cases {
		r := prepared(t)
		c.edit(r)
		_, err := r.run(t)
		e := envelopeOf(t, err)
		if e.Code != c.code || e.ReviewerStarted {
			t.Fatalf("%s: envelope %+v, want %s", name, e, c.code)
		}
		if usedRecords(t, r.root) != 0 || len(r.mgr.launched) != 0 {
			t.Fatalf("%s: consumed=%d launched=%d", name, usedRecords(t, r.root), len(r.mgr.launched))
		}
	}
}

func TestRunReview_AMappingChangedAfterPrepareIsRefused(t *testing.T) {
	r := prepared(t)
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("f", 64), Argv: []string{"--json"}, EgressPolicyID: "none",
	}
	writeJSON(t, filepath.Join(r.root, "review", "maps", "pi-v1.json"), m)
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodeMappingMismatch || usedRecords(t, r.root) != 0 || len(r.mgr.launched) != 0 {
		t.Fatalf("envelope %+v consumed=%d", e, usedRecords(t, r.root))
	}
}

func TestRunReview_ALeaseTamperedWithSinceItWasSealedIsRefused(t *testing.T) {
	r := prepared(t)
	p := filepath.Join(r.root, "review", "leases", r.args.Lease, "evidence.json")
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"citations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodeAttestationInvalid || len(r.mgr.launched) != 0 {
		t.Fatalf("envelope %+v", e)
	}
}

func TestRunReview_WithoutAGuestBackendOrOnAShareBackendNothingStarts(t *testing.T) {
	r := prepared(t)
	r.svc.WithRunner(RunnerConfig{Manager: r.mgr, Backend: model.BackendLibkrun})
	_, err := r.run(t)
	if e := envelopeOf(t, err); e.Code != reviewprep.CodePolicyUnenforcable || len(r.mgr.launched) != 0 || usedRecords(t, r.root) != 0 {
		t.Fatalf("envelope %+v", e)
	}
	r2 := prepared(t)
	r2.svc.WithRunner(RunnerConfig{})
	_, err = r2.run(t)
	if e := envelopeOf(t, err); e.Code != reviewprep.CodeProfileUnavailable {
		t.Fatalf("envelope %+v", e)
	}
}

func TestRunReview_ATimeoutIsRefusedWithTheReviewerStartedAndTheGuestRemoved(t *testing.T) {
	r := prepared(t)
	r.mgr.runRes = func(model.ExecRequest) (*model.ExecResult, error) { return nil, context.DeadlineExceeded }
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodeRunTimeout || !e.ReviewerStarted || len(r.mgr.removed) != 1 {
		t.Fatalf("envelope %+v removed=%v", e, r.mgr.removed)
	}
}

func TestRunReview_TooMuchOutputIsRefusedAndNoneOfItIsReturned(t *testing.T) {
	r := prepared(t)
	r.args.MaxOutputBytes = 10
	r.mgr.runRes = func(model.ExecRequest) (*model.ExecResult, error) {
		return &model.ExecResult{Stdout: []byte("123456"), Stderr: []byte("789012")}, nil
	}
	raw, err := r.svc.RunReview(context.Background(), r.args)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodeOutputLimit || !e.ReviewerStarted || raw != nil || strings.Contains(e.Detail, "123456") {
		t.Fatalf("envelope %+v raw=%q", e, raw)
	}
	// Exactly at the cap is allowed.
	r2 := prepared(t)
	r2.args.MaxOutputBytes = 12
	r2.mgr.runRes = func(model.ExecRequest) (*model.ExecResult, error) {
		return &model.ExecResult{Stdout: []byte("123456"), Stderr: []byte("789012")}, nil
	}
	if _, err := r2.run(t); err != nil {
		t.Fatalf("output at the cap refused: %v", err)
	}
}

func TestRunReview_TheLimitsDefaultAndAreHandedToTheGuest(t *testing.T) {
	r := prepared(t)
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r.mgr.execs[len(r.mgr.execs)-1].Timeout; got != controlproto.DefaultReviewSeconds*1e9 {
		t.Fatalf("default limit %v", got)
	}
	r2 := prepared(t)
	r2.args.MaxSeconds = 7
	if _, err := r2.run(t); err != nil {
		t.Fatal(err)
	}
	if got := r2.mgr.execs[len(r2.mgr.execs)-1].Timeout; got != 7*1e9 {
		t.Fatalf("limit %v", got)
	}
}

func TestRunReview_ALauncherRefusalIsTheRunsRefusalWithTheReviewerNotStarted(t *testing.T) {
	r := prepared(t)
	r.mgr.runRes = func(req model.ExecRequest) (*model.ExecResult, error) {
		nonce := strings.TrimPrefix(req.Env[0], "MGIT_REVIEW_REFUSAL_NONCE=")
		env, _ := json.Marshal(map[string]any{"schema": "mgit.review-error/v1", "code": "HASH_MISMATCH", "phase": "launch",
			"reviewer_started": false, "detail": "d", "nonce": nonce})
		return &model.ExecResult{ExitCode: 70, Stderr: append(env, '\n')}, nil
	}
	_, err := r.run(t)
	if e := envelopeOf(t, err); e.Code != "HASH_MISMATCH" || e.ReviewerStarted || len(r.mgr.removed) != 1 {
		t.Fatalf("envelope %+v", e)
	}
}

func TestRunReview_ASealThatFailsRefusesBeforeTheReviewerAndRemovesTheGuest(t *testing.T) {
	r := prepared(t)
	r.mgr.sealRes = &model.ExecResult{ExitCode: 1, Stderr: []byte("did not take")}
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodePolicyUnenforcable || e.ReviewerStarted || len(r.mgr.removed) != 1 {
		t.Fatalf("envelope %+v removed=%v", e, r.mgr.removed)
	}
	for _, x := range r.mgr.execs {
		if len(x.Env) > 0 {
			t.Fatal("the reviewer ran after a failed seal")
		}
	}
}

func TestRunReview_TheBindingsAreAttachedAfterTheSealAndReleasedWhateverHappens(t *testing.T) {
	r := prepared(t)
	b := &recBindings{}
	r.svc.WithRunner(RunnerConfig{Manager: r.mgr, Backend: model.BackendKVM, Bindings: b})
	r.mgr.runRes = func(model.ExecRequest) (*model.ExecResult, error) { return nil, context.DeadlineExceeded }
	_, err := r.run(t)
	if envelopeOf(t, err).Code != reviewprep.CodeRunTimeout {
		t.Fatal(err)
	}
	if len(b.attached) != 1 || b.released != 1 {
		t.Fatalf("attached=%d released=%d", len(b.attached), b.released)
	}
	in := b.attached[0]
	if in.GuestID != "sb-run" || in.Mapping.ID != "pi-v1" || in.Attestation.Lease != r.args.Lease || in.Exec == nil {
		t.Fatalf("attach input %+v", in)
	}
}

func TestRunReview_ABindingsRefusalStopsTheRunBeforeTheReviewer(t *testing.T) {
	r := prepared(t)
	b := &recBindings{err: reviewprep.Refuse(reviewprep.CodePolicyUnenforcable, "no credential slot")}
	r.svc.WithRunner(RunnerConfig{Manager: r.mgr, Backend: model.BackendKVM, Bindings: b})
	_, err := r.run(t)
	e := envelopeOf(t, err)
	if e.Code != reviewprep.CodePolicyUnenforcable || e.ReviewerStarted || b.released != 0 || len(r.mgr.removed) != 1 {
		t.Fatalf("envelope %+v released=%d", e, b.released)
	}
	for _, x := range r.mgr.execs {
		if len(x.Env) > 0 {
			t.Fatal("the reviewer ran after bindings refused")
		}
	}
}

func TestRunReview_ByDefaultAMappingThatNeedsACredentialIsRefused(t *testing.T) {
	r := prepared(t)
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"},
		EgressPolicyID: "none", CredentialRef: "cred-1",
	}
	writeJSON(t, filepath.Join(r.root, "review", "maps", "pi-v1.json"), m)
	_, err := r.run(t)
	// The attestation was made without a credential, so the changed mapping is
	// caught first; a mapping that asks for one from the start is refused too.
	if envelopeOf(t, err).Code != reviewprep.CodeMappingMismatch {
		t.Fatal(err)
	}
}
