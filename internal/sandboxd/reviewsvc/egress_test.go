package reviewsvc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// svcEgress is the host egress stack as the run service sees it. Refs: MGIT-344
type svcEgress struct {
	started, stopped, revoked []string
	granted                   []string
}

func (e *svcEgress) StartEgress(_ context.Context, info model.SandboxInfo) error {
	e.started = append(e.started, info.ID)
	return nil
}
func (e *svcEgress) StopEgress(id string) { e.stopped = append(e.stopped, id) }
func (e *svcEgress) AllowEgress(_ context.Context, id, entry string) error {
	e.granted = append(e.granted, id+" "+entry)
	return nil
}
func (e *svcEgress) RevokeAll(id string) { e.revoked = append(e.revoked, id) }

// preparedWithEgress prepares a lease whose mapping names the given egress
// policy, and wires a runner with the given host egress stack (nil for none).
func preparedWithEgress(t *testing.T, policy string, eg *svcEgress) (*runEnv, *recBindings) {
	t.Helper()
	e := setup(t)
	m := reviewprep.Mapping{
		Schema: reviewprep.MapSchema, ID: "pi-v1", BindingSHA256: strings.Repeat("b", 64), ImageDigest: image,
		GuestEntrypoint: "/opt/r/bin/r", EntrypointSHA256: strings.Repeat("e", 64), Argv: []string{"--json"}, EgressPolicyID: policy,
	}
	writeJSON(t, filepath.Join(e.root, "review", "maps", "pi-v1.json"), m)
	raw, err := e.svc.PrepareReview(context.Background(), e.args)
	if err != nil {
		t.Fatal(err)
	}
	var att reviewprep.Attestation
	if err := json.Unmarshal(raw, &att); err != nil {
		t.Fatal(err)
	}
	mgr := &runMgr{backend: model.BackendKVM}
	b := &recBindings{}
	cfg := RunnerConfig{Manager: mgr, Backend: model.BackendKVM, Bindings: b}
	if eg != nil {
		cfg.Egress = eg
	}
	e.svc.WithRunner(cfg)
	return &runEnv{env: e, mgr: mgr, args: controlproto.ReviewRunArgs{
		Profile: e.args.Profile, Image: e.args.Image, MappingID: e.args.MappingID, BindingDigest: e.args.BindingDigest,
		BundleDigest: e.args.BundleDigest, Nonce: e.args.Nonce, Lease: att.Lease,
	}}, b
}

// A mapping that names an egress policy, on a daemon that has a host egress
// stack, gets an allowlist guest with its stack up, and the bindings are handed
// a grant function that reaches this guest's stack. The stack is revoked and
// stopped when the run ends. Refs: MGIT-344
func TestRunReview_AMappingThatNamesEgressGetsAnAllowlistGuestAndAGrantFunction(t *testing.T) {
	eg := &svcEgress{}
	r, b := preparedWithEgress(t, "allow-docs", eg)
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if o := r.mgr.launched[0]; o.Network.Mode != model.NetworkModeAllowlist || len(o.Network.Allowlist) != 0 {
		t.Fatalf("network %+v", o.Network)
	}
	if len(b.attached) != 1 || b.attached[0].AllowEgress == nil {
		t.Fatal("the bindings were not handed a grant function")
	}
	if err := b.attached[0].AllowEgress(context.Background(), "192.0.2.7:443"); err != nil {
		t.Fatal(err)
	}
	if len(eg.started) != 1 || len(eg.granted) != 1 || eg.granted[0] != "sb-run 192.0.2.7:443" ||
		len(eg.revoked) != 1 || len(eg.stopped) != 1 {
		t.Fatalf("egress %+v", eg)
	}
}

// Without both halves the guest stays without a network and the bindings get
// no grant function, so a binding that needs egress refuses instead of running
// without it. Refs: MGIT-344
func TestRunReview_NoEgressUnlessTheMappingAsksAndTheDaemonHasAStack(t *testing.T) {
	for name, c := range map[string]struct {
		policy string
		eg     *svcEgress
	}{
		"mapping asks, no stack":      {"allow-docs", nil},
		"stack, mapping says none":    {"none", &svcEgress{}},
		"stack, mapping says nothing": {"", &svcEgress{}},
	} {
		r, b := preparedWithEgress(t, c.policy, c.eg)
		_, _ = r.run(t)
		if len(r.mgr.launched) != 1 || r.mgr.launched[0].Network.Mode != model.NetworkModeNone {
			t.Fatalf("%s: launches %+v", name, r.mgr.launched)
		}
		if len(b.attached) != 1 || b.attached[0].AllowEgress != nil {
			t.Fatalf("%s: the bindings were handed a grant function", name)
		}
		if c.eg != nil && len(c.eg.started) != 0 {
			t.Fatalf("%s: a stack was started: %+v", name, c.eg)
		}
	}
}
