package reviewguest

import (
	"context"
	"errors"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// fakeEgress records each call to the host egress stack, with how far the
// guest had got (execs made, removals made) when it came, so a test can read
// the order against the manager's own record. Refs: MGIT-344
type fakeEgress struct {
	mgr      *fakeMgr
	calls    []string
	at       [][2]int // len(mgr.execs), len(mgr.removed) at each call
	started  []model.SandboxInfo
	granted  []string
	startErr error
	grantErr error
}

func (e *fakeEgress) note(c string) {
	e.calls = append(e.calls, c)
	e.at = append(e.at, [2]int{len(e.mgr.execs), len(e.mgr.removed)})
}

func (e *fakeEgress) StartEgress(_ context.Context, info model.SandboxInfo) error {
	e.note("start:" + info.ID)
	e.started = append(e.started, info)
	return e.startErr
}
func (e *fakeEgress) StopEgress(id string) { e.note("stop:" + id) }
func (e *fakeEgress) AllowEgress(_ context.Context, id, entry string) error {
	e.note("allow:" + id)
	e.granted = append(e.granted, entry)
	return e.grantErr
}
func (e *fakeEgress) RevokeAll(id string) { e.note("revoke:" + id) }

func egressSpec(t *testing.T, m *fakeMgr) (Spec, *fakeEgress) {
	t.Helper()
	e := &fakeEgress{mgr: m}
	s := spec(t)
	s.Egress = e
	return s, e
}

// A guest that is given an egress stack launches in allowlist mode with an
// empty list (deny everything until a grant), and the host stack is up before
// anything runs in the guest, the seal included. Refs: MGIT-344
func TestProvision_WithEgressLaunchesAnEmptyAllowlistAndStartsTheHostStackFirst(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s, e := egressSpec(t, m)
	g, err := Provision(context.Background(), m, s)
	if err != nil {
		t.Fatal(err)
	}
	o := m.launched[0]
	if o.Network.Mode != model.NetworkModeAllowlist || len(o.Network.Allowlist) != 0 {
		t.Fatalf("network %+v, want allowlist mode with nothing on the list", o.Network)
	}
	if len(e.calls) != 1 || e.calls[0] != "start:sb1" || e.at[0] != [2]int{0, 0} {
		t.Fatalf("egress calls %v at %v, want one start before any exec", e.calls, e.at)
	}
	if e.started[0].NetworkMode != model.NetworkModeAllowlist {
		t.Fatalf("the stack was started for mode %q", e.started[0].NetworkMode)
	}
	if g.ID != "sb1" {
		t.Fatalf("guest %q", g.ID)
	}
}

// A host stack that will not start leaves no guest behind and refuses before
// anything runs. Refs: MGIT-344
func TestProvision_AHostStackThatWillNotStartRemovesTheGuestAndRefuses(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s, e := egressSpec(t, m)
	e.startErr = errors.New("no tap")
	_, err := Provision(context.Background(), m, s)
	var re *reviewprep.Error
	if !errors.As(err, &re) || re.Code != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("err %v, want a POLICY_UNENFORCEABLE refusal", err)
	}
	if len(m.execs) != 0 || len(m.removed) != 1 {
		t.Fatalf("execs=%d removed=%v, want nothing run and the guest removed", len(m.execs), m.removed)
	}
	if last := e.calls[len(e.calls)-1]; last != "stop:sb1" || e.at[len(e.at)-1][1] != 0 {
		t.Fatalf("egress calls %v at %v, want the stack stopped before the guest is removed", e.calls, e.at)
	}
}

// A seal that fails on an egress guest takes the host stack down before the
// guest is removed. Refs: MGIT-344
func TestProvision_ASealFailureOnAnEgressGuestStopsTheStackBeforeRemoving(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM, res: &model.ExecResult{ExitCode: 1}}
	s, e := egressSpec(t, m)
	if _, err := Provision(context.Background(), m, s); err == nil {
		t.Fatal("a failed seal was accepted")
	}
	want := []string{"start:sb1", "revoke:sb1", "stop:sb1"}
	if len(e.calls) != 3 || e.calls[1] != want[1] || e.calls[2] != want[2] {
		t.Fatalf("egress calls %v, want %v", e.calls, want)
	}
	if e.at[2][1] != 0 || len(m.removed) != 1 {
		t.Fatalf("stop came at %v with removed=%v, want before the removal", e.at[2], m.removed)
	}
}

// A grant goes to this guest's own stack; a guest with no egress refuses a
// grant instead of running without it. Refs: MGIT-344
func TestGuest_AGrantReachesItsOwnStackAndAGuestWithoutEgressRefusesIt(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s, e := egressSpec(t, m)
	g, err := Provision(context.Background(), m, s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.AllowEgress(context.Background(), "192.0.2.7:443"); err != nil {
		t.Fatal(err)
	}
	if len(e.granted) != 1 || e.granted[0] != "192.0.2.7:443" || e.calls[len(e.calls)-1] != "allow:sb1" {
		t.Fatalf("granted %v calls %v", e.granted, e.calls)
	}
	e.grantErr = errors.New("not an ip")
	var re *reviewprep.Error
	if err := g.AllowEgress(context.Background(), "example.com:443"); !errors.As(err, &re) || re.Code != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("a refused grant came back as %v", err)
	}

	plain := &fakeMgr{backend: model.BackendKVM}
	pg, err := Provision(context.Background(), plain, spec(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.AllowEgress(context.Background(), "192.0.2.7:443"); !errors.As(err, &re) || re.Code != reviewprep.CodePolicyUnenforcable {
		t.Fatalf("a guest with no egress accepted a grant: %v", err)
	}
	if pg.HasEgress() || !g.HasEgress() {
		t.Fatalf("HasEgress: plain=%v egress=%v", pg.HasEgress(), g.HasEgress())
	}
}

// Closing an egress guest revokes every grant and stops the stack before the
// guest is removed, so no grant outlives the guest. Refs: MGIT-344
func TestGuest_CloseRevokesAndStopsTheStackBeforeRemoving(t *testing.T) {
	m := &fakeMgr{backend: model.BackendKVM}
	s, e := egressSpec(t, m)
	g, err := Provision(context.Background(), m, s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := len(e.calls)
	if n < 3 || e.calls[n-2] != "revoke:sb1" || e.calls[n-1] != "stop:sb1" {
		t.Fatalf("egress calls %v, want revoke then stop last", e.calls)
	}
	if e.at[n-1][1] != 0 || len(m.removed) != 1 {
		t.Fatalf("stop came at %v with removed=%v, want before the removal", e.at[n-1], m.removed)
	}
}
