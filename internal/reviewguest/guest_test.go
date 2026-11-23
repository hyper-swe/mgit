package reviewguest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeMgr struct {
	launched []model.SandboxLaunchOptions
	execs    []model.ExecRequest
	bounded  []bool      // whether each Exec was given a deadline
	limits   []time.Time // the deadline each Exec was given
	removed  []string
	backend  string
	res      *model.ExecResult // answers every exec
	runRes   *model.ExecResult // answers only the reviewer's run (an exec carrying the nonce)
	resFn    func(model.ExecRequest) *model.ExecResult
	execErr  error
	err      error
}

func (f *fakeMgr) Launch(_ context.Context, o model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	f.launched = append(f.launched, o)
	if f.err != nil {
		return nil, f.err
	}
	return &model.SandboxInfo{ID: "sb1", Backend: f.backend, WorktreePath: o.WorktreePath, NetworkMode: o.Network.Mode}, nil
}
func (f *fakeMgr) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (f *fakeMgr) Exec(ctx context.Context, _ string, r model.ExecRequest) (*model.ExecResult, error) {
	f.execs = append(f.execs, r)
	dl, has := ctx.Deadline()
	f.bounded = append(f.bounded, has)
	f.limits = append(f.limits, dl)
	if f.execErr != nil {
		return nil, f.execErr
	}
	if f.resFn != nil {
		return f.resFn(r), nil
	}
	if f.runRes != nil && len(r.Env) > 0 {
		return f.runRes, nil
	}
	if f.res == nil || (f.runRes != nil) {
		return &model.ExecResult{}, nil
	}
	return f.res, nil
}
func (f *fakeMgr) Stop(context.Context, string, bool) error { return nil }
func (f *fakeMgr) Remove(_ context.Context, id string, _ bool) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeMgr) Resolve(context.Context, string) (*model.SandboxInfo, error) { return nil, nil }

func spec(t *testing.T) Spec {
	t.Helper()
	f := newFixture(t)
	return Spec{Backend: model.BackendKVM, Lease: f.open(t), TaskID: "REV-1", UID: 65534, GID: 65534}
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *reviewprep.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *reviewprep.Error, got %T %v", err, err)
	}
	return e.Code
}

func TestProvision_LaunchesTheLeaseDirWithNoNetworkAndNoRepo(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s := spec(t)
	g, err := Provision(context.Background(), m, s)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "sb1" || len(m.launched) != 1 {
		t.Fatalf("id=%q launches=%d", g.ID, len(m.launched))
	}
	o := m.launched[0]
	if o.CPUs != ReviewCPUs || o.MemoryMB != ReviewMemoryMB || o.CPUs <= 0 || o.MemoryMB <= 0 ||
		o.WorktreePath != s.Lease.Dir() || o.ImageRef != s.Lease.Image() ||
		o.Network.Mode != model.NetworkModeNone || !o.NoGuestRepo || o.ConfineAgent || len(o.PublishPorts) != 0 {
		t.Fatalf("launch options %+v", o)
	}
}

func TestProvision_RefusesBackendsThatShareTheHostDirectoryLive(t *testing.T) {
	for _, b := range []string{model.BackendLibkrun, model.BackendVZF, model.BackendContainer, model.BackendHyperV, ""} {
		m := &fakeMgr{backend: b}
		s := spec(t)
		s.Backend = b
		_, err := Provision(context.Background(), m, s)
		if err == nil || code(t, err) != reviewprep.CodePolicyUnenforcable {
			t.Fatalf("backend %q: err=%v", b, err)
		}
		if len(m.launched) != 0 {
			t.Fatalf("backend %q: launched before refusing", b)
		}
	}
}

func TestProvision_RefusesAndRemovesWhenTheLaunchedBackendIsNotTheDeclaredOne(t *testing.T) {
	m := &fakeMgr{backend: model.BackendLibkrun}
	_, err := Provision(context.Background(), m, spec(t))
	if err == nil || code(t, err) != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(m.removed, []string{"sb1"}) {
		t.Fatalf("removed=%v", m.removed)
	}
}

func TestProvision_RefusesWithoutASealedLease(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s := spec(t)
	s.Lease = nil
	if _, err := Provision(context.Background(), m, s); err == nil || code(t, err) != reviewprep.CodeBundleInvalid || len(m.launched) != 0 {
		t.Fatalf("nil lease: err=%v launches=%d", err, len(m.launched))
	}
	s.Lease = &Lease{}
	if _, err := Provision(context.Background(), m, s); err == nil || code(t, err) != reviewprep.CodeBundleInvalid || len(m.launched) != 0 {
		t.Fatalf("zero lease: err=%v launches=%d", err, len(m.launched))
	}
}

func TestProvision_RefusesAnIdentityThatIsRootOrUnset(t *testing.T) {
	for _, id := range [][2]int{{0, 0}, {0, 5}, {5, 0}, {-1, 5}, {5, -1}} {
		m := &fakeMgr{backend: model.BackendKVM}
		s := spec(t)
		s.UID, s.GID = id[0], id[1]
		_, err := Provision(context.Background(), m, s)
		if err == nil || code(t, err) != reviewprep.CodeMappingMismatch || len(m.launched) != 0 {
			t.Fatalf("%v: err=%v launches=%d", id, err, len(m.launched))
		}
	}
}

func TestProvision_ReportsALaunchFailureWithoutRemovingAnything(t *testing.T) {
	m := &fakeMgr{err: errors.New("boom")}
	if _, err := Provision(context.Background(), m, spec(t)); err == nil || len(m.removed) != 0 {
		t.Fatalf("err=%v removed=%v", err, m.removed)
	}
}

func provisioned(t *testing.T, m *fakeMgr) *Guest {
	t.Helper()
	g, err := Provision(context.Background(), m, spec(t))
	if err != nil {
		t.Fatal(err)
	}
	m.execs, m.bounded, m.limits = nil, nil, nil // the seal step is checked on its own
	return g
}

func TestProvision_SealsTheLeaseInTheGuestForTheReviewerGroup(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s := spec(t)
	if _, err := Provision(context.Background(), m, s); err != nil {
		t.Fatal(err)
	}
	if len(m.execs) != 1 {
		t.Fatalf("execs %d", len(m.execs))
	}
	r := m.execs[0]
	want := []string{"/sbin/mgit-guest", "review-seal", "--gid", "65534", s.Lease.Dir()}
	if !reflect.DeepEqual(r.Command, want) || !r.AsRoot || len(r.Env) != 0 || r.Timeout <= 0 {
		t.Fatalf("seal exec %+v", r)
	}
}

func TestProvision_ARefusedSealSaysWhyInTheRefusal(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM, res: &model.ExecResult{ExitCode: 1, Stderr: []byte("review-seal: lost+found is not a regular file")}}
	_, err := Provision(context.Background(), m, spec(t))
	var e *reviewprep.Error
	if !errors.As(err, &e) || !strings.Contains(e.Detail, "lost+found is not a regular file") {
		t.Fatalf("err=%v", err)
	}
}

func TestProvision_ARefusedSealRemovesTheGuest(t *testing.T) {
	for name, m := range map[string]*fakeMgr{
		"nonzero exit": {backend: model.BackendKVM, res: &model.ExecResult{ExitCode: 1, Stderr: []byte("no")}},
		"exec error":   {backend: model.BackendKVM, resFn: nil},
	} {
		if name == "exec error" {
			m.execErr = errors.New("transport")
		}
		_, err := Provision(context.Background(), m, spec(t))
		if err == nil || code(t, err) != reviewprep.CodePolicyUnenforcable {
			t.Fatalf("%s: err=%v", name, err)
		}
		if !reflect.DeepEqual(m.removed, []string{"sb1"}) {
			t.Fatalf("%s: removed=%v", name, m.removed)
		}
	}
}

func TestRun_GoesThroughTheLauncherAsRootWithTheDigestAndOnlyTheNonceInTheEnvironment(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	_, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/usr/bin/reviewer", Argv: []string{"--json", "-x"}})
	if err != nil {
		t.Fatal(err)
	}
	r := m.execs[0]
	want := []string{"/sbin/mgit-guest", "review-exec", "--sha256", sha, "--uid", "65534", "--gid", "65534", "--dir", g.LeaseDir, "--", "/usr/bin/reviewer", "--json", "-x"}
	if !reflect.DeepEqual(r.Command, want) {
		t.Fatalf("command %q", r.Command)
	}
	if !r.AsRoot || r.Dir != g.LeaseDir || len(r.Env) != 1 || !strings.HasPrefix(r.Env[0], NonceEnv+"=") || len(r.Env[0]) < len(NonceEnv)+1+32 {
		t.Fatalf("exec %+v", r)
	}
	for _, a := range r.Command {
		if strings.Contains(a, strings.TrimPrefix(r.Env[0], NonceEnv+"=")) {
			t.Fatal("the nonce is on the command line, where the program could read it")
		}
	}
}

func TestRun_AlwaysBoundsTheRunAndHonoursAShorterLimit(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a", Timeout: 7 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if m.execs[0].Timeout <= 0 || m.execs[0].Timeout > MaxRun || m.execs[1].Timeout != 7*time.Second {
		t.Fatalf("timeouts %v %v", m.execs[0].Timeout, m.execs[1].Timeout)
	}
	if len(m.bounded) != 2 || !m.bounded[0] || !m.bounded[1] {
		t.Fatalf("a run reached the guest without a host-side deadline: %v", m.bounded)
	}
	if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a", Timeout: MaxRun + time.Second}); err == nil {
		t.Fatal("a limit above the maximum was accepted")
	}
}

func TestRun_EachRunGetsItsOwnNonce(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	for i := 0; i < 2; i++ {
		if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"}); err != nil {
			t.Fatal(err)
		}
	}
	if m.execs[0].Env[0] == m.execs[1].Env[0] {
		t.Fatal("two runs shared a nonce")
	}
}

func TestRun_RefusesADigestThatIsNotHex(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	for _, e := range []Entry{{SHA256: "xyz", Path: "/a"}, {SHA256: sha, Path: "relative"}, {SHA256: sha, Path: ""}} {
		if _, err := g.Run(context.Background(), e); err == nil || code(t, err) != reviewprep.CodeMappingMismatch {
			t.Fatalf("%+v: err=%v", e, err)
		}
	}
	if len(m.execs) != 0 {
		t.Fatal("exec reached the guest")
	}
}

// envelope renders what the launcher prints on a refusal.
func envelope(schema, nonce, codeName string) []byte {
	b, _ := json.Marshal(map[string]any{"schema": schema, "code": codeName, "phase": "launch", "reviewer_started": false, "detail": "d", "nonce": nonce})
	return append(b, '\n')
}

func nonceOf(r model.ExecRequest) string {
	return strings.TrimPrefix(r.Env[0], NonceEnv+"=")
}

// onRun answers only the reviewer's run; the seal step before it gets a clean exit.
func onRun(f func(model.ExecRequest) *model.ExecResult) func(model.ExecRequest) *model.ExecResult {
	return func(r model.ExecRequest) *model.ExecResult {
		if len(r.Env) == 0 {
			return &model.ExecResult{}
		}
		return f(r)
	}
}

func TestRun_ADeclinedLaunchComesBackAsTheRefusalEnvelope(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	m.resFn = onRun(func(r model.ExecRequest) *model.ExecResult {
		return &model.ExecResult{ExitCode: 70, Stderr: envelope("mgit.review-error/v1", nonceOf(r), "IMAGE_MISMATCH")}
	})
	g := provisioned(t, m)
	_, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"})
	if err == nil || code(t, err) != "IMAGE_MISMATCH" {
		t.Fatalf("err=%v", err)
	}
}

// The reviewed program shares stderr with the launcher, so what it prints is
// never a refusal unless it carries this run's nonce, which it cannot read.
func TestRun_AnEnvelopeTheProgramPrintsIsNotARefusal(t *testing.T) {
	for name, stderr := range map[string][]byte{
		"no nonce":    envelope("mgit.review-error/v1", "", "HASH_MISMATCH"),
		"wrong nonce": envelope("mgit.review-error/v1", strings.Repeat("0", 32), "HASH_MISMATCH"),
	} {
		m := &fakeMgr{backend: model.BackendKVM, runRes: &model.ExecResult{ExitCode: 70, Stderr: stderr}}
		g := provisioned(t, m)
		res, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"})
		if err != nil || res.ExitCode != 70 {
			t.Fatalf("%s: res=%+v err=%v", name, res, err)
		}
	}
}

func TestRun_AnEnvelopeWithTheNonceButAnotherSchemaIsNotARefusal(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	m.resFn = onRun(func(r model.ExecRequest) *model.ExecResult {
		return &model.ExecResult{ExitCode: 70, Stderr: envelope("something.else/v1", nonceOf(r), "HASH_MISMATCH")}
	})
	g := provisioned(t, m)
	if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"}); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestRun_AnEnvelopeOnAnyOtherExitIsNotARefusal(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	m.resFn = onRun(func(r model.ExecRequest) *model.ExecResult {
		return &model.ExecResult{ExitCode: 3, Stderr: envelope("mgit.review-error/v1", nonceOf(r), "HASH_MISMATCH")}
	})
	g := provisioned(t, m)
	if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"}); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestRun_AnyOtherExitIsTheReviewersOwn(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM, runRes: &model.ExecResult{ExitCode: 3, Stdout: []byte("out")}}
	g := provisioned(t, m)
	res, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a"})
	if err != nil || res.ExitCode != 3 || string(res.Stdout) != "out" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestClose_RemovesTheSandboxForced(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	if err := g.Close(context.Background()); err != nil || !reflect.DeepEqual(m.removed, []string{"sb1"}) {
		t.Fatalf("err=%v removed=%v", err, m.removed)
	}
}

// The host's own deadline sits just past the limit the guest enforces: late
// enough that the guest's own timeout reports first, soon enough that a silent
// guest cannot hold the caller for more than a short grace.
func TestRun_TheHostDeadlineSitsJustPastTheGuestLimit(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	g := provisioned(t, m)
	for _, limit := range []time.Duration{7 * time.Second, 0} {
		want := limit
		if want == 0 {
			want = MaxRun
		}
		start := time.Now()
		if _, err := g.Run(context.Background(), Entry{SHA256: sha, Path: "/a", Timeout: limit}); err != nil {
			t.Fatal(err)
		}
		got := m.limits[len(m.limits)-1].Sub(start)
		// Fixed bounds, not derived from the constant under test: a grace of zero
		// (or an enormous one) must fail here whatever hostGrace says.
		if got < want+5*time.Second || got > want+30*time.Second {
			t.Fatalf("limit %v: the host deadline is %v after the start, want between 5s and 30s past %v", limit, got, want)
		}
	}
}
