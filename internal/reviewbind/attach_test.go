package reviewbind

import (
	"context"
	"errors"
	"net/netip"
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

// fakeEgress stands for the daemon's egress stack: it records the exact
// ip:port entries granted and can fail on one.
type fakeEgress struct {
	entries []string
	failOn  int // 1-based grant that fails
}

func (f *fakeEgress) allow(_ context.Context, entry string) error {
	f.entries = append(f.entries, entry)
	if f.failOn == len(f.entries) {
		return errors.New("grant refused")
	}
	return nil
}

func lookupOf(addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, _ string) ([]netip.Addr, error) {
		out := make([]netip.Addr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func src(secret string) CredentialSource {
	return func(_ context.Context, ref string) (*Slot, error) {
		s := NewSlot(ref)
		err := s.Fill(strings.NewReader(secret))
		return s, err
	}
}

// attach runs Attach; eg nil means the daemon gave the guest no egress stack.
func attach(t *testing.T, b reviewsvc.Bindings, m reviewprep.Mapping, e *execRec, eg *fakeEgress) (func(context.Context) error, error) {
	t.Helper()
	in := reviewsvc.AttachInput{GuestID: "g1", Mapping: m, Exec: e.run}
	if eg != nil {
		in.AllowEgress = eg.allow
	}
	rel, err := b.Attach(context.Background(), in)
	if rel == nil {
		t.Fatal("release must never be nil")
	}
	return rel, err
}

func hostMap() reviewprep.Mapping {
	return reviewprep.Mapping{CredentialRef: "review-key", EgressPolicyID: "host:api.example.com"}
}

func TestAttach_NothingAskedInstallsNothing(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	rel, err := attach(t, New(Config{}), reviewprep.Mapping{EgressPolicyID: "none"}, e, eg)
	if err != nil || len(e.reqs) != 0 || len(eg.entries) != 0 {
		t.Fatalf("err=%v execs=%d grants=%v", err, len(e.reqs), eg.entries)
	}
	if rel(context.Background()) != nil {
		t.Fatal("release")
	}
}

func TestAttach_CredentialIsInstalledOverStdinNeverArgv(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	b := New(Config{ReviewerGID: 4242, Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7")})
	rel, err := attach(t, b, hostMap(), e, eg)
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
	if err := rel(context.Background()); err != nil || len(e.reqs) != 2 || !strings.Contains(strings.Join(e.reqs[1].Command, " "), "cred-remove") {
		t.Fatalf("release did not remove the slot: err=%v reqs=%d", err, len(e.reqs))
	}
	if rel(context.Background()) != nil || len(e.reqs) != 2 {
		t.Fatal("release must be idempotent")
	}
}

func TestAttach_GrantsEveryResolvedAddressOnPort443Only(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	b := New(Config{Source: src("x"), Lookup: lookupOf("203.0.113.7", "203.0.113.8", "2001:db8::1")})
	if _, err := attach(t, b, hostMap(), e, eg); err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7:443", "203.0.113.8:443", "[2001:db8::1]:443"}
	if strings.Join(eg.entries, ",") != strings.Join(want, ",") {
		t.Fatalf("grants %v, want %v", eg.entries, want)
	}
	for _, g := range eg.entries {
		if !strings.HasSuffix(g, ":443") {
			t.Fatalf("a grant is not on port 443: %s", g)
		}
	}
}

func TestAttach_HostEgressWithNoStackIsRefusedBeforeAnythingRuns(t *testing.T) {
	e := &execRec{}
	_, err := attach(t, New(Config{Source: src("x"), Lookup: lookupOf("203.0.113.7")}), hostMap(), e, nil)
	assertRefused(t, err)
	if len(e.reqs) != 0 {
		t.Fatal("nothing may be installed before a refusal")
	}
}

func TestAttach_CredentialWithoutSourceIsRefused(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	_, err := attach(t, New(Config{Lookup: lookupOf("203.0.113.7")}), hostMap(), e, eg)
	assertRefused(t, err)
}

func TestAttach_ANameThatResolvesToNothingIsRefused(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	_, err := attach(t, New(Config{Source: src("x"), Lookup: lookupOf()}), hostMap(), e, eg)
	assertRefused(t, err)
	if len(eg.entries) != 0 || len(e.reqs) != 0 {
		t.Fatal("something was granted or installed for a name with no address")
	}
}

func TestAttach_ALookupFailureIsRefused(t *testing.T) {
	failing := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	_, err := attach(t, New(Config{Source: src("x"), Lookup: failing}), hostMap(), &execRec{}, &fakeEgress{})
	assertRefused(t, err)
}

func TestAttach_AGrantFailureIsRefusedBeforeTheCredentialIsInstalled(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{failOn: 2}
	b := New(Config{Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7", "203.0.113.8")})
	_, err := attach(t, b, hostMap(), e, eg)
	assertRefused(t, err)
	if len(e.reqs) != 0 {
		t.Fatal("the credential was installed although a grant failed")
	}
	if strings.Contains(err.Error(), "203.0.113") {
		t.Fatal("the refusal names an address")
	}
}

func TestAttach_InstallFailureIsRefusedAndNoSecretIsInTheError(t *testing.T) {
	e, eg := &execRec{fail: 1}, &fakeEgress{}
	b := New(Config{Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7")})
	rel, err := attach(t, b, hostMap(), e, eg)
	assertRefused(t, err)
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatal("secret in error")
	}
	_ = rel(context.Background())
	_ = rel(context.Background())
}

func installed(t *testing.T, e *execRec) func(context.Context) error {
	t.Helper()
	b := New(Config{Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7")})
	rel, err := attach(t, b, hostMap(), e, &fakeEgress{})
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestRelease_ANonzeroCleanupExitIsAnError(t *testing.T) {
	err := installed(t, &execRec{code: map[int]int{2: 1}})(context.Background())
	if err == nil {
		t.Fatal("a cleanup that exited nonzero was reported as success")
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatal("secret in the cleanup error")
	}
}

func TestRelease_ANilCleanupResultIsAnError(t *testing.T) {
	if err := installed(t, &execRec{nilAt: 2})(context.Background()); err == nil {
		t.Fatal("a cleanup with no result was reported as success")
	}
}

func TestAttach_ANonzeroCredInstallExitIsRefused(t *testing.T) {
	e := &execRec{code: map[int]int{1: 3}}
	b := New(Config{Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7")})
	rel, err := attach(t, b, hostMap(), e, &fakeEgress{})
	assertRefused(t, err)
	_ = rel(context.Background())
	if len(e.reqs) != 1 {
		t.Fatalf("execs %d, want only the failed install", len(e.reqs))
	}
}

func TestAttach_ACredentialWithoutAHostIsRefusedBeforeAnythingRuns(t *testing.T) {
	e, eg := &execRec{}, &fakeEgress{}
	b := New(Config{Source: src("s3cr3t-value"), Lookup: lookupOf("203.0.113.7")})
	_, err := attach(t, b, reviewprep.Mapping{CredentialRef: "k", EgressPolicyID: "none"}, e, eg)
	assertRefused(t, err)
	if len(e.reqs) != 0 || len(eg.entries) != 0 {
		t.Fatal("something ran before the refusal")
	}
}

func assertRefused(t *testing.T, err error) {
	t.Helper()
	var pe *reviewprep.Error
	if !errors.As(err, &pe) || pe.Code != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("want POLICY_UNENFORCEABLE refusal, got %v", err)
	}
}
