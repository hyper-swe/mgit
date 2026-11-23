package reviewbind

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

type execRec struct {
	reqs  []model.ExecRequest
	stdin []string
	fail  int         // 1-based call that fails
	code  map[int]int // 1-based call -> nonzero exit code
	nilAt int         // 1-based call that returns a nil result with no error
}

func (e *execRec) run(_ context.Context, r model.ExecRequest) (*model.ExecResult, error) {
	e.reqs = append(e.reqs, r)
	e.stdin = append(e.stdin, string(r.Stdin))
	if e.fail == len(e.reqs) {
		return nil, errors.New("exec failed")
	}
	if e.nilAt == len(e.reqs) {
		return nil, nil
	}
	return &model.ExecResult{ExitCode: e.code[len(e.reqs)]}, nil
}

type fakeEgress struct{ applied, released int }

func (f *fakeEgress) Apply(_ context.Context, _ string, _ Policy) (func(context.Context) error, error) {
	f.applied++
	return func(context.Context) error { f.released++; return nil }, nil
}

func src(secret string) CredentialSource {
	return func(_ context.Context, ref string) (*Slot, error) {
		s := NewSlot(ref)
		err := s.Fill(strings.NewReader(secret))
		return s, err
	}
}

func attach(t *testing.T, b reviewsvc.Bindings, m reviewprep.Mapping, e *execRec) (func(context.Context) error, error) {
	t.Helper()
	rel, err := b.Attach(context.Background(), reviewsvc.AttachInput{GuestID: "g1", Mapping: m, Exec: e.run})
	if rel == nil {
		t.Fatal("release must never be nil")
	}
	return rel, err
}

func TestAttach_NothingAskedInstallsNothing(t *testing.T) {
	e := &execRec{}
	rel, err := attach(t, New(Config{}), reviewprep.Mapping{EgressPolicyID: "none"}, e)
	if err != nil || len(e.reqs) != 0 {
		t.Fatalf("err=%v execs=%d", err, len(e.reqs))
	}
	if rel(context.Background()) != nil {
		t.Fatal("release")
	}
}

func TestAttach_CredentialIsInstalledOverStdinNeverArgv(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	b := New(Config{ReviewerGID: 4242, Source: src("s3cr3t-value"), Egress: eg})
	m := reviewprep.Mapping{CredentialRef: "review-key", EgressPolicyID: "host:api.example.com"}
	rel, err := attach(t, b, m, e)
	if err != nil || len(e.reqs) != 1 {
		t.Fatalf("err=%v execs=%d", err, len(e.reqs))
	}
	r := e.reqs[0]
	if e.stdin[0] != "s3cr3t-value" {
		t.Fatal("secret did not travel on stdin")
	}
	all := strings.Join(append(append([]string{}, r.Command...), r.Env...), " ")
	if strings.Contains(all, "s3cr3t-value") {
		t.Fatal("secret in argv/env")
	}
	for _, want := range []string{"cred-install", "/run/review-cred/review-key", "4242"} {
		if !strings.Contains(all, want) {
			t.Fatalf("command %q lacks %q", all, want)
		}
	}
	if eg.applied != 1 {
		t.Fatal("egress policy not applied")
	}
	if err := rel(context.Background()); err != nil || len(e.reqs) != 2 || !strings.Contains(strings.Join(e.reqs[1].Command, " "), "cred-remove") || eg.released != 1 {
		t.Fatalf("release did not remove and lift: err=%v reqs=%d rel=%d", err, len(e.reqs), eg.released)
	}
	if rel(context.Background()) != nil || len(e.reqs) != 2 || eg.released != 1 {
		t.Fatal("release must be idempotent")
	}
}

func TestAttach_HostEgressWithoutApplierIsRefused(t *testing.T) {
	e := &execRec{}
	_, err := attach(t, New(Config{Source: src("x")}), reviewprep.Mapping{EgressPolicyID: "host:api.example.com"}, e)
	assertRefused(t, err)
	if len(e.reqs) != 0 {
		t.Fatal("nothing may be installed before a refusal")
	}
}

func TestAttach_CredentialWithoutSourceIsRefused(t *testing.T) {
	e := &execRec{}
	_, err := attach(t, New(Config{Egress: &fakeEgress{}}), reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "host:a.example.com"}, e)
	assertRefused(t, err)
}

func TestAttach_InstallFailureUndoesEgressAndReleaseIsSafe(t *testing.T) {
	e, eg := &execRec{fail: 1}, &fakeEgress{}
	b := New(Config{Source: src("s3cr3t-value"), Egress: eg})
	rel, err := attach(t, b, reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "host:a.example.com"}, e)
	assertRefused(t, err)
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatal("secret in error")
	}
	_ = rel(context.Background())
	_ = rel(context.Background())
	if eg.released != 1 {
		t.Fatalf("egress released %d times, want 1", eg.released)
	}
}

func assertRefused(t *testing.T, err error) {
	t.Helper()
	var pe *reviewprep.Error
	if !errors.As(err, &pe) || pe.Code != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("want POLICY_UNENFORCEABLE refusal, got %v", err)
	}
}

func installed(t *testing.T, e *execRec, eg *fakeEgress) func(context.Context) error {
	t.Helper()
	b := New(Config{Source: src("s3cr3t-value"), Egress: eg})
	rel, err := attach(t, b, reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "host:a.example.com"}, e)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestRelease_ANonzeroCleanupExitIsAnErrorAndStillLiftsThePolicy(t *testing.T) {
	e, eg := &execRec{code: map[int]int{2: 1}}, &fakeEgress{}
	err := installed(t, e, eg)(context.Background())
	if err == nil {
		t.Fatal("a cleanup that exited nonzero was reported as success")
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatal("secret in the cleanup error")
	}
	if eg.released != 1 {
		t.Fatalf("egress lifted %d times, want 1", eg.released)
	}
}

func TestRelease_ANilCleanupResultIsAnErrorAndStillLiftsThePolicy(t *testing.T) {
	e, eg := &execRec{nilAt: 2}, &fakeEgress{}
	if err := installed(t, e, eg)(context.Background()); err == nil {
		t.Fatal("a cleanup with no result was reported as success")
	}
	if eg.released != 1 {
		t.Fatalf("egress lifted %d times, want 1", eg.released)
	}
}

func TestAttach_ANonzeroCredInstallExitIsRefusedAndNothingIsLeftBehind(t *testing.T) {
	e, eg := &execRec{code: map[int]int{1: 3}}, &fakeEgress{}
	b := New(Config{Source: src("s3cr3t-value"), Egress: eg})
	rel, err := attach(t, b, reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "host:a.example.com"}, e)
	assertRefused(t, err)
	_ = rel(context.Background())
	if eg.released != 1 || len(e.reqs) != 1 {
		t.Fatalf("lifted %d, execs %d", eg.released, len(e.reqs))
	}
}

func TestAttach_ACredentialWithoutAHostIsRefusedBeforeAnythingRuns(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	b := New(Config{Source: src("s3cr3t-value"), Egress: eg})
	_, err := attach(t, b, reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "none"}, e)
	assertRefused(t, err)
	if len(e.reqs) != 0 || eg.applied != 0 {
		t.Fatal("something ran before the refusal")
	}
}
