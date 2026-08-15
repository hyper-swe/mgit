package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakePolicyController stands in for whatever is actually enforcing egress —
// the in-daemon runner on firecracker, the VM child over the control channel
// on libkrun. The service must behave identically against both.
type fakePolicyController struct {
	mu       sync.Mutex
	setCalls int
	gotID    string
	gotEntry []string
	gotDrain bool
	change   model.EgressPolicyChange
	state    model.EgressPolicyState
	setErr   error
	showErr  error
}

func (f *fakePolicyController) SetEgressPolicy(
	_ context.Context, sandboxID string, entries []string, drain bool,
) (model.EgressPolicyChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.gotID, f.gotEntry, f.gotDrain = sandboxID, entries, drain
	if f.setErr != nil {
		return model.EgressPolicyChange{}, f.setErr
	}
	return f.change, nil
}

func (f *fakePolicyController) EgressPolicy(
	_ context.Context, sandboxID string,
) (model.EgressPolicyState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotID = sandboxID
	if f.showErr != nil {
		return model.EgressPolicyState{}, f.showErr
	}
	return f.state, nil
}

// recordingEvents captures the append-only audit trail.
//
// failFrom makes the sink start failing at the Nth call (1-based); 0 disables
// it. That is needed to reach the case where the "requested" record lands but
// the "applied" one does not — a sink that fails on every call blocks the
// mutation at the first record and never exercises the second.
type recordingEvents struct {
	mu       sync.Mutex
	events   []model.SandboxEvent
	err      error
	failFrom int
	calls    int
}

func (r *recordingEvents) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil && (r.failFrom == 0 || r.calls >= r.failFrom) {
		return r.err
	}
	r.events = append(r.events, *ev)
	return nil
}

func (r *recordingEvents) snapshot() []model.SandboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.SandboxEvent(nil), r.events...)
}

func policyTestSandbox() model.SandboxInfo {
	return model.SandboxInfo{ID: "01SB", TaskID: "MGIT-72", NetworkMode: model.NetworkModeAllowlist}
}

func newPolicyService(t *testing.T, ctrl EgressPolicyController, events SandboxEventAppender) *EgressPolicyService {
	t.Helper()
	// A stager that always reports the sandbox booted keeps these cases on the
	// LIVE enforcer path, which is what they are about. The pending-launch
	// route has its own file. Refs: MGIT-109
	svc, err := NewEgressPolicyService(ctrl, &fakePendingStager{stageErr: model.ErrSandboxBooted,
		pendingErr: model.ErrSandboxBooted}, events,
		func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	return svc
}

// TestEgressPolicyService_Set_AppliesAndAudits is the happy path: the
// mutation reaches the enforcing side, and the append-only trail names the
// task binding and what actually changed. Refs: MGIT-72, FR-17.18
func TestEgressPolicyService_Set_AppliesAndAudits(t *testing.T) {
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{
		Entries: []string{"registry.npmjs.org:443"}, RuleCount: 1, Killed: 2,
	}}
	events := &recordingEvents{}
	svc := newPolicyService(t, ctrl, events)

	got, err := svc.Set(context.Background(), policyTestSandbox(), []string{"registry.npmjs.org:443"}, false)

	require.NoError(t, err)
	assert.Equal(t, []string{"registry.npmjs.org:443"}, got.Entries)
	assert.Equal(t, 2, got.Killed)
	assert.Equal(t, "01SB", ctrl.gotID)

	recorded := events.snapshot()
	require.Len(t, recorded, 2, "a mutation is audited both as requested and as applied")
	for _, ev := range recorded {
		assert.Equal(t, model.EventPolicyChanged, ev.EventType)
		assert.Equal(t, "01SB", ev.SandboxID)
		assert.Equal(t, "MGIT-72", ev.TaskID, "the audit record must name the task binding")
	}
	assert.Contains(t, recorded[0].Detail, `"phase":"requested"`)
	assert.Contains(t, recorded[1].Detail, `"phase":"applied"`)
	assert.Contains(t, recorded[1].Detail, `"established_flows_killed":2`)
	assert.Contains(t, recorded[1].Detail, "registry.npmjs.org:443")
}

// TestEgressPolicyService_Revoke_KillsByDefault verifies the default carries
// the strong guarantee: an unqualified revoke terminates established flows.
// Refs: MGIT-72, ADR-012
func TestEgressPolicyService_Revoke_KillsByDefault(t *testing.T) {
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{Killed: 1}}
	svc := newPolicyService(t, ctrl, &recordingEvents{})

	_, err := svc.Set(context.Background(), policyTestSandbox(), nil, false)

	require.NoError(t, err)
	assert.False(t, ctrl.gotDrain, "kill is the default; drain must be asked for by name")
}

// TestEgressPolicyService_Revoke_DrainIsOptIn is the matching positive
// control: the weaker behavior is reachable, but only when requested.
func TestEgressPolicyService_Revoke_DrainIsOptIn(t *testing.T) {
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{Drained: true}}
	svc := newPolicyService(t, ctrl, &recordingEvents{})

	got, err := svc.Set(context.Background(), policyTestSandbox(), nil, true)

	require.NoError(t, err)
	assert.True(t, ctrl.gotDrain)
	assert.True(t, got.Drained)
}

// TestEgressPolicyService_Set_ControllerFails_IsAuditedAsNotApplied is the
// reason the trail records outcomes and not only intentions: a failed
// mutation must not leave a record claiming the policy changed. A caller who
// believes egress is closed when it is open is the whole hazard.
// Refs: MGIT-72, FR-17.18
func TestEgressPolicyService_Set_ControllerFails_IsAuditedAsNotApplied(t *testing.T) {
	ctrl := &fakePolicyController{setErr: errors.New("vm control channel unreachable")}
	events := &recordingEvents{}
	svc := newPolicyService(t, ctrl, events)

	_, err := svc.Set(context.Background(), policyTestSandbox(), nil, false)

	require.Error(t, err)
	recorded := events.snapshot()
	require.Len(t, recorded, 2)
	assert.Contains(t, recorded[0].Detail, `"phase":"requested"`)
	assert.Contains(t, recorded[1].Detail, `"phase":"failed"`)
	assert.NotContains(t, recorded[1].Detail, `"phase":"applied"`)
}

// TestEgressPolicyService_Set_AuditFailureBlocksTheMutation verifies the
// fail-closed order: if the append-only record cannot be written, the policy
// is NOT changed. An unrecorded widening is authority nobody can reconstruct.
// Refs: MGIT-72, FR-17.18
func TestEgressPolicyService_Set_AuditFailureBlocksTheMutation(t *testing.T) {
	ctrl := &fakePolicyController{}
	events := &recordingEvents{err: errors.New("disk full")}
	svc := newPolicyService(t, ctrl, events)

	_, err := svc.Set(context.Background(), policyTestSandbox(), []string{"evil.example:443"}, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit")
	assert.Equal(t, 0, ctrl.setCalls, "no policy may take effect before it is on the record")
}

// TestEgressPolicyService_Set_AppliedAuditFails_ReportsTheChangeAndTheError
// covers the awkward middle case: the "requested" record lands, the enforcer
// APPLIES the change, and then the "applied" record fails to write.
//
// The change is real and cannot be taken back, so the service must hand back
// BOTH the change and an error saying the record is missing. Returning a bare
// error with no change would tell the caller nothing happened while egress had
// in fact just been mutated — and that is the one reading of this path that
// gets someone hurt. Refs: MGIT-72, FR-17.18, SEC-04
func TestEgressPolicyService_Set_AppliedAuditFails_ReportsTheChangeAndTheError(t *testing.T) {
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{
		Entries: []string{"a.example:443"}, RuleCount: 1, Killed: 2,
	}}
	// Let the "requested" record through; fail the "applied" one.
	events := &recordingEvents{err: errors.New("disk full"), failFrom: 2}
	svc := newPolicyService(t, ctrl, events)

	change, err := svc.Set(context.Background(), policyTestSandbox(), []string{"a.example:443"}, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "WAS changed",
		"the error must say the mutation took effect, not merely that auditing failed")
	require.NotNil(t, change,
		"the change is real and unrecoverable; withholding it would read as 'nothing happened'")
	assert.Equal(t, 2, change.Killed)
	assert.Equal(t, 1, ctrl.setCalls)
}

// TestEgressPolicyService_Set_NoNetworkSandbox_IsRefused verifies a sandbox
// with no egress stack is an ERROR, not a silent success. "Revoke succeeded"
// for a sandbox that was never enforcing would be the most dangerous possible
// lie. Refs: MGIT-72, SEC-04
func TestEgressPolicyService_Set_NoNetworkSandbox_IsRefused(t *testing.T) {
	ctrl := &fakePolicyController{}
	svc := newPolicyService(t, ctrl, &recordingEvents{})
	info := policyTestSandbox()
	info.NetworkMode = model.NetworkModeNone

	_, err := svc.Set(context.Background(), info, []string{"a.example:443"}, false)

	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "none")
	assert.Equal(t, 0, ctrl.setCalls)
}

// TestEgressPolicyService_Set_OpenNetworkSandbox_IsRefused covers the OTHER
// non-allowlist mode, and it is the more dangerous of the two.
//
// A `none` sandbox that refuses a revoke leaves the caller no worse off — there
// was no egress to begin with. An `open` sandbox has UNRESTRICTED egress and no
// allowlist to narrow, so a revoke that reported success there would leave the
// caller running untrusted code with the whole network reachable, believing
// they had just closed it. That is the exact failure the fail-closed guard
// exists for, so it gets its own assertion rather than riding on the `none`
// case. Refs: MGIT-72, SEC-04
func TestEgressPolicyService_Set_OpenNetworkSandbox_IsRefused(t *testing.T) {
	ctrl := &fakePolicyController{}
	svc := newPolicyService(t, ctrl, &recordingEvents{})
	info := policyTestSandbox()
	info.NetworkMode = model.NetworkModeOpen

	_, err := svc.Set(context.Background(), info, []string{"a.example:443"}, false)

	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "allowlist",
		"the refusal must name the mode requirement, so a caller knows to relaunch "+
			"rather than assume egress was narrowed")
	assert.Equal(t, 0, ctrl.setCalls,
		"nothing may reach the enforcer for a sandbox with no allowlist to mutate")
}

// TestEgressPolicyService_Show_NonAllowlistSandbox_IsRefused holds the READ
// path to the same fail-closed rule as the write path.
//
// An empty policy and an unenforced one are opposite facts that look identical
// in an empty list: if `show` answered a non-allowlist sandbox with zero
// entries, a caller confirming their revoke would read "nothing is allowed"
// where the truth is "nothing is enforcing". Refs: MGIT-72, SEC-04
func TestEgressPolicyService_Show_NonAllowlistSandbox_IsRefused(t *testing.T) {
	for _, mode := range []string{model.NetworkModeNone, model.NetworkModeOpen} {
		t.Run(mode, func(t *testing.T) {
			svc := newPolicyService(t, &fakePolicyController{
				state: model.EgressPolicyState{Entries: []string{"stale.example:443"}},
			}, &recordingEvents{})
			info := policyTestSandbox()
			info.NetworkMode = mode

			got, err := svc.Show(context.Background(), info)

			require.Error(t, err)
			assert.Nil(t, got, "a refused read must not hand back a policy to misread")
		})
	}
}

// TestEgressPolicyService_Show_ControllerFails_IsReported verifies a read that
// cannot reach the enforcer is an ERROR, not an empty policy — the same reason
// as above: a caller must never be shown "no egress permitted" when the truth
// is "could not ask". Refs: MGIT-72, SEC-04
func TestEgressPolicyService_Show_ControllerFails_IsReported(t *testing.T) {
	ctrl := &fakePolicyController{showErr: errors.New("no running egress stack")}
	svc := newPolicyService(t, ctrl, &recordingEvents{})

	got, err := svc.Show(context.Background(), policyTestSandbox())

	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "no running egress stack",
		"the enforcer's reason must survive to the caller, not be flattened")
}

// TestEgressPolicyService_Show_ReportsTheLivePolicy verifies the read side
// answers with what is being enforced now, which after a mutation is not the
// launch policy. Refs: MGIT-72
func TestEgressPolicyService_Show_ReportsTheLivePolicy(t *testing.T) {
	ctrl := &fakePolicyController{state: model.EgressPolicyState{
		Entries: []string{"a.example:443"}, RuleCount: 1,
	}}
	svc := newPolicyService(t, ctrl, &recordingEvents{})

	got, err := svc.Show(context.Background(), policyTestSandbox())

	require.NoError(t, err)
	assert.Equal(t, []string{"a.example:443"}, got.Entries)
	assert.Equal(t, "01SB", ctrl.gotID)
}

// TestEgressPolicyService_Show_IsNotAudited verifies a read leaves no trail
// entry: an audit log padded with reads is one nobody reviews.
func TestEgressPolicyService_Show_IsNotAudited(t *testing.T) {
	events := &recordingEvents{}
	svc := newPolicyService(t, &fakePolicyController{}, events)

	_, err := svc.Show(context.Background(), policyTestSandbox())

	require.NoError(t, err)
	assert.Empty(t, events.snapshot())
}

// TestNewEgressPolicyService_RequiresItsCollaborators verifies the service
// cannot be built without an enforcer, an audit sink and a clock — a service
// missing any of them would fail open at exactly the wrong moment.
func TestNewEgressPolicyService_RequiresItsCollaborators(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0).UTC() }
	tests := []struct {
		name   string
		ctrl   EgressPolicyController
		stager PendingPolicyStager
		events SandboxEventAppender
		clock  func() time.Time
	}{
		{name: "nil_controller", stager: &fakePendingStager{}, events: &recordingEvents{}, clock: clock},
		{name: "nil_stager", ctrl: &fakePolicyController{}, events: &recordingEvents{}, clock: clock},
		{name: "nil_events", ctrl: &fakePolicyController{}, stager: &fakePendingStager{}, clock: clock},
		{name: "nil_clock", ctrl: &fakePolicyController{}, stager: &fakePendingStager{}, events: &recordingEvents{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewEgressPolicyService(tt.ctrl, tt.stager, tt.events, tt.clock)
			require.Error(t, err)
		})
	}
}
