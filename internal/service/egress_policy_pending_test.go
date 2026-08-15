package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakePendingStager stands in for the sandbox registry's pending-launch view:
// the thing that knows whether a sandbox's VM has booted and owns the launch
// options it will boot with.
type fakePendingStager struct {
	stageCalls  int
	showCalls   int
	gotID       string
	gotEntries  []string
	staged      model.EgressPolicyState
	pending     model.EgressPolicyState
	stageErr    error
	pendingErr  error
	stagedAfter bool // set when a stage actually mutated something
}

func (f *fakePendingStager) StagePendingEgressPolicy(
	_ context.Context, sandboxID string, entries []string,
) (model.EgressPolicyState, error) {
	f.stageCalls++
	f.gotID, f.gotEntries = sandboxID, entries
	if f.stageErr != nil {
		return model.EgressPolicyState{}, f.stageErr
	}
	f.stagedAfter = true
	return f.staged, nil
}

func (f *fakePendingStager) PendingEgressPolicy(
	_ context.Context, sandboxID string,
) (model.EgressPolicyState, error) {
	f.showCalls++
	f.gotID = sandboxID
	if f.pendingErr != nil {
		return model.EgressPolicyState{}, f.pendingErr
	}
	return f.pending, nil
}

func newPendingPolicyService(
	t *testing.T, ctrl EgressPolicyController, stager PendingPolicyStager, events SandboxEventAppender,
) *EgressPolicyService {
	t.Helper()
	svc, err := NewEgressPolicyService(ctrl, stager, events,
		func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	return svc
}

// createdSandbox is the state a `mgit work --sandbox` registration is in
// immediately after the documented setup step: registered, allowlist mode, and
// with no microVM yet (lazy provisioning). Refs: FR-17.9, FR-17.10
func createdSandbox() model.SandboxInfo {
	return model.SandboxInfo{
		ID: "01SB", TaskID: "MGIT-109",
		NetworkMode: model.NetworkModeAllowlist, State: model.StateCreated,
	}
}

func runningSandbox() model.SandboxInfo {
	info := createdSandbox()
	info.State = model.StateRunning
	return info
}

// TestEgressPolicyService_Set_CreatedSandbox_StagesOntoPendingLaunch is the
// reported defect (MGIT-109): the egress-policy verbs dialed a VM that lazy
// provisioning had deliberately not booted, so the documented setup path failed
// with "vm control channel unreachable".
//
// The policy is STAGED onto the pending launch rather than applied to a VM that
// booted just to be reconfigured — which is also strictly safer, because the VM
// then never runs under the policy the caller was replacing, not even briefly.
// Refs: MGIT-109, FR-17.9, FR-17.10
func TestEgressPolicyService_Set_CreatedSandbox_StagesOntoPendingLaunch(t *testing.T) {
	ctrl := &fakePolicyController{setErr: errors.New("must not be dialed")}
	stager := &fakePendingStager{staged: model.EgressPolicyState{
		Entries: []string{"other.example.com:443"}, Pending: true,
	}}
	events := &recordingEvents{}
	svc := newPendingPolicyService(t, ctrl, stager, events)

	got, err := svc.Set(context.Background(), createdSandbox(), []string{"other.example.com:443"}, false)

	require.NoError(t, err)
	assert.Equal(t, 1, stager.stageCalls)
	assert.Equal(t, 0, ctrl.setCalls, "a sandbox with no VM must not be dialed at all")
	assert.Equal(t, "01SB", stager.gotID, "the mutation is keyed by the host-anchored sandbox ID")
	assert.Equal(t, []string{"other.example.com:443"}, got.Entries)
	assert.True(t, got.Pending, "a staged policy must never be reported as one in force")
	assert.Zero(t, got.Killed, "a VM that has never run has no established flows to kill")
}

// TestEgressPolicyService_Set_CreatedSandbox_AuditsAsStagedNotApplied keeps the
// append-only trail honest: a record saying a running policy was APPLIED, for a
// sandbox whose VM has not booted, is the same lie the Pending field exists to
// prevent — one a reviewer reconstructing what was enforced would believe.
// Refs: MGIT-109, FR-17.18
func TestEgressPolicyService_Set_CreatedSandbox_AuditsAsStagedNotApplied(t *testing.T) {
	stager := &fakePendingStager{staged: model.EgressPolicyState{
		Entries: []string{"other.example.com:443"}, Pending: true,
	}}
	events := &recordingEvents{}
	svc := newPendingPolicyService(t, &fakePolicyController{}, stager, events)

	_, err := svc.Set(context.Background(), createdSandbox(), []string{"other.example.com:443"}, false)
	require.NoError(t, err)

	recorded := events.snapshot()
	require.Len(t, recorded, 2, "a staged mutation is audited both as requested and as staged")
	assert.Contains(t, recorded[0].Detail, `"phase":"requested"`)
	assert.Contains(t, recorded[1].Detail, `"phase":"staged"`)
	assert.Contains(t, recorded[1].Detail, `"pending":true`)
	assert.NotContains(t, recorded[1].Detail, `"phase":"applied"`,
		"nothing was applied: no enforcer has seen this policy yet")
}

// TestEgressPolicyService_Show_CreatedSandbox_ReportsPendingNotEnforced is the
// "records are truth" half. `show` on a registered-but-unbooted sandbox must
// report the policy that WILL be enforced, labeled as not yet in force —
// never presented as live, and never as an empty policy (which would read as
// "nothing is permitted" when the truth is "nothing is enforcing yet").
// Refs: MGIT-109, MGIT-72, SEC-04
func TestEgressPolicyService_Show_CreatedSandbox_ReportsPendingNotEnforced(t *testing.T) {
	ctrl := &fakePolicyController{showErr: errors.New("must not be dialed")}
	stager := &fakePendingStager{pending: model.EgressPolicyState{
		Entries: []string{"example.com:443"},
	}}
	svc := newPendingPolicyService(t, ctrl, stager, &recordingEvents{})

	got, err := svc.Show(context.Background(), createdSandbox())

	require.NoError(t, err)
	assert.Equal(t, 1, stager.showCalls)
	assert.Equal(t, []string{"example.com:443"}, got.Entries)
	assert.True(t, got.Pending, "a policy nothing is enforcing yet must say so")
	assert.Zero(t, got.RuleCount, "nothing has compiled these entries yet")
}

// TestEgressPolicyService_Set_SuspendedSandbox_StagesOntoPendingLaunch covers
// the second not-booted state. Idle-suspend stops the VM and leaves the
// registration to re-launch from the same options on next use, so a suspended
// sandbox has no live enforcer either — and staging is exactly right, because
// the resume boots from the options being staged onto. Refs: MGIT-109, NFR-17.3
func TestEgressPolicyService_Set_SuspendedSandbox_StagesOntoPendingLaunch(t *testing.T) {
	info := createdSandbox()
	info.State = model.StateSuspended
	stager := &fakePendingStager{staged: model.EgressPolicyState{Pending: true}}
	ctrl := &fakePolicyController{}
	svc := newPendingPolicyService(t, ctrl, stager, &recordingEvents{})

	got, err := svc.Set(context.Background(), info, nil, false)

	require.NoError(t, err)
	assert.Equal(t, 1, stager.stageCalls)
	assert.Equal(t, 0, ctrl.setCalls)
	assert.True(t, got.Pending)
}

// TestEgressPolicyService_Set_BootedDuringStage_ReroutesToLiveEnforcer covers
// the race the whole ErrSandboxBooted sentinel exists for.
//
// A boot landing between "read the recorded state" and "stage the policy" would
// otherwise end with a staged policy reported as ENFORCED while the running VM
// kept the old one — precisely the lie the fail-closed contract prevents. The
// stager refuses, having staged nothing, and the caller re-routes to the live
// enforcer. Refs: MGIT-109, MGIT-72, SEC-04
func TestEgressPolicyService_Set_BootedDuringStage_ReroutesToLiveEnforcer(t *testing.T) {
	stager := &fakePendingStager{stageErr: model.ErrSandboxBooted}
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{
		Entries: []string{"other.example.com:443"}, RuleCount: 1, Killed: 3,
	}}
	events := &recordingEvents{}
	svc := newPendingPolicyService(t, ctrl, stager, events)

	got, err := svc.Set(context.Background(), createdSandbox(), []string{"other.example.com:443"}, false)

	require.NoError(t, err)
	assert.Equal(t, 1, stager.stageCalls)
	assert.False(t, stager.stagedAfter, "the lost race must stage NOTHING before it refuses")
	assert.Equal(t, 1, ctrl.setCalls, "the change belongs on the live enforcer now")
	assert.False(t, got.Pending, "this policy IS in force; reporting it pending would understate it")
	assert.Equal(t, 3, got.Killed)

	recorded := events.snapshot()
	require.Len(t, recorded, 2)
	assert.Contains(t, recorded[1].Detail, `"phase":"applied"`,
		"the re-routed change was really applied, and the trail says so")
	assert.NotContains(t, recorded[1].Detail, `"phase":"staged"`)
}

// TestEgressPolicyService_Show_BootedDuringShow_ReadsLiveEnforcer is the read
// side of the same race: a sandbox that booted after its state was read must be
// reported from the enforcer, not from the pending launch options.
// Refs: MGIT-109
func TestEgressPolicyService_Show_BootedDuringShow_ReadsLiveEnforcer(t *testing.T) {
	stager := &fakePendingStager{pendingErr: model.ErrSandboxBooted}
	ctrl := &fakePolicyController{state: model.EgressPolicyState{
		Entries: []string{"example.com:443"}, RuleCount: 1,
	}}
	svc := newPendingPolicyService(t, ctrl, stager, &recordingEvents{})

	got, err := svc.Show(context.Background(), createdSandbox())

	require.NoError(t, err)
	assert.Equal(t, []string{"example.com:443"}, got.Entries)
	assert.False(t, got.Pending, "this policy IS being enforced")
	assert.Equal(t, 1, got.RuleCount)
}

// TestEgressPolicyService_Set_StagerFails_FailsClosed keeps the contract whole
// at the new seam: a stage that could not be made durable must be an error, not
// a cheerful success. A caller told the policy was staged, whose VM then boots
// with the wider launch-time allowlist, is running untrusted code under a policy
// they believe they replaced. Refs: MGIT-109, SEC-04
func TestEgressPolicyService_Set_StagerFails_FailsClosed(t *testing.T) {
	stager := &fakePendingStager{stageErr: errors.New("registry write failed")}
	events := &recordingEvents{}
	svc := newPendingPolicyService(t, &fakePolicyController{}, stager, events)

	got, err := svc.Set(context.Background(), createdSandbox(), []string{"other.example.com:443"}, false)

	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "registry write failed")
	recorded := events.snapshot()
	require.Len(t, recorded, 2)
	assert.Contains(t, recorded[1].Detail, `"phase":"failed"`,
		"a failed stage must not leave a record claiming the policy changed")
}

// TestEgressPolicyService_UnreachableEnforcer_NamesTheConditionThatApplies is
// the diagnostic half of MGIT-109.
//
// One string used to serve three genuinely different failures as a two-cause
// guess ("the sandbox may not be running, or its VM predates this capability").
// Each has a different remedy, and the shrug is why a live bug report was
// misattributed to two unrelated tickets. The daemon holds the recorded state
// and the enforcer reports host-side evidence; between them the condition is
// known, so it is named.
//
// The NEGATIVES are the point: a never-booted sandbox is never told its VM
// predates anything, and a dead guest is never told it was never booted.
// Refs: MGIT-109, MGIT-104, MGIT-99, MGIT-74
func TestEgressPolicyService_UnreachableEnforcer_NamesTheConditionThatApplies(t *testing.T) {
	unreachable := func(vmStateSeen, channelSeen bool) error {
		return &model.EgressChannelUnreachableError{
			SocketPath: "/w/01SB/c.sock", VMStateSeen: vmStateSeen, ChannelSeen: channelSeen,
			Cause: errors.New("connect: no such file or directory"),
		}
	}
	tests := []struct {
		name        string
		state       string
		stageErr    error
		liveErr     error
		wantCode    string
		wantPhrases []string
		denyPhrases []string
	}{
		{
			// The pending route is taken, and staging itself fails. The
			// sandbox condition is still "no VM", which is what an integrator
			// branches on; the prose names what actually went wrong.
			name:        "never_booted",
			state:       model.StateCreated,
			stageErr:    errors.New("registry write failed"),
			wantCode:    model.EgressFailureNotBooted,
			wantPhrases: []string{"has NOT booted", "current policy"},
			denyPhrases: []string{"predates", "exited or was killed"},
		},
		{
			name:        "booted_then_unreachable",
			state:       model.StateRunning,
			liveErr:     unreachable(true, true),
			wantCode:    model.EgressFailureBootedDied,
			wantPhrases: []string{"recorded as running", "exited or was killed"},
			denyPhrases: []string{"predates", "has NOT booted"},
		},
		{
			name:        "vm_state_gone_entirely",
			state:       model.StateRunning,
			liveErr:     unreachable(false, false),
			wantCode:    model.EgressFailureBootedDied,
			wantPhrases: []string{"recorded as running", "exited or was killed"},
			denyPhrases: []string{"predates", "has NOT booted"},
		},
		{
			name:        "predates_the_capability",
			state:       model.StateRunning,
			liveErr:     unreachable(true, false),
			wantCode:    model.EgressFailureVersionPredates,
			wantPhrases: []string{"predates", "relaunch"},
			denyPhrases: []string{"has NOT booted", "exited or was killed"},
		},
		{
			// A state this build does not recognize gets its OWN token, never
			// the nearest of the other three.
			name:        "unrecognized_state",
			state:       "teleported",
			liveErr:     unreachable(true, true),
			wantCode:    model.EgressFailureUnknown,
			wantPhrases: []string{"cannot reason about"},
			denyPhrases: []string{"predates", "has NOT booted", "exited or was killed"},
		},
		{
			// Not an unreachable enforcer at all: a refusal from a reachable
			// one. Still coded, still UNKNOWN rather than a guess.
			name:        "enforcer_refusal",
			state:       model.StateRunning,
			liveErr:     errors.New("rule \"::\" does not compile"),
			wantCode:    model.EgressFailureUnknown,
			wantPhrases: []string{"does not compile"},
			denyPhrases: []string{"predates", "has NOT booted", "exited or was killed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := createdSandbox()
			info.State = tt.state
			ctrl := &fakePolicyController{setErr: tt.liveErr}
			stageErr := tt.stageErr
			if stageErr == nil {
				// Force the live path for the running/unrecognized cases.
				stageErr = model.ErrSandboxBooted
			}
			svc := newPendingPolicyService(t, ctrl, &fakePendingStager{stageErr: stageErr},
				&recordingEvents{})

			_, err := svc.Set(context.Background(), info, nil, false)

			require.Error(t, err)
			var failure *model.EgressPolicyError
			require.ErrorAs(t, err, &failure,
				"every policy-verb failure carries a machine-readable code")
			assert.Equal(t, tt.wantCode, failure.Code)

			got := err.Error()
			assert.Contains(t, got, "["+tt.wantCode+"]",
				"the token is readable from a bare stderr line too")
			for _, want := range tt.wantPhrases {
				assert.Contains(t, got, want)
			}
			for _, deny := range tt.denyPhrases {
				assert.NotContains(t, got, deny,
					"a %s failure must not be described as something else", tt.name)
			}
			assert.NotContains(t, got, "may not be running, or",
				"the two-cause guess is what sent this bug to the wrong ticket")
		})
	}
}

// TestEgressFailureCodes_AreAStableContract pins the exact token strings.
//
// THIS TEST IS THE POINT OF THE TOKENS. An integrator built a pre-boot retry by
// matching on the error WORDING; it silently missed the not-booted failure, and
// the reword this ticket ships would have broken it a second time just as
// silently. Pinning the tokens here is what makes the prose above them free to
// improve forever after: change any message you like, and this test stays
// green; change a token, and it fails loudly, because that IS an API break.
//
// The set is CLOSED. Adding a member is a deliberate act that shows up here.
// Refs: MGIT-109, R-H233
func TestEgressFailureCodes_AreAStableContract(t *testing.T) {
	// Golden set. These strings are a published contract; the prose beside
	// them in the error messages is NOT.
	golden := map[string]string{
		"not booted":       "NOT_BOOTED",
		"booted then died": "BOOTED_DIED",
		"predates":         "VERSION_PREDATES",
		"unclassified":     "UNKNOWN",
	}
	assert.Equal(t, golden["not booted"], model.EgressFailureNotBooted)
	assert.Equal(t, golden["booted then died"], model.EgressFailureBootedDied)
	assert.Equal(t, golden["predates"], model.EgressFailureVersionPredates)
	assert.Equal(t, golden["unclassified"], model.EgressFailureUnknown)

	for _, code := range golden {
		assert.True(t, model.ValidEgressFailureCode(code),
			"%q must be a member of the closed vocabulary", code)
	}
	assert.False(t, model.ValidEgressFailureCode("NOT_RUNNING"),
		"the set is closed: a plausible-looking token is not a member")
	assert.False(t, model.ValidEgressFailureCode(""),
		"an empty code is never valid; an unclassifiable failure is UNKNOWN")
	assert.Len(t, golden, 4, "adding a token is a contract change and must be deliberate")
}

// TestEgressPolicyService_Set_RunningSandbox_UnreachableStillFailsClosed is the
// regression guard on the contract MGIT-109 must not weaken: making `set`
// succeed against a NOT-YET-BOOTED sandbox is only safe if a genuinely
// unreachable RUNNING enforcer is still an error with the running policy
// unchanged. Refs: MGIT-109, MGIT-72, ADR-012, SEC-04
func TestEgressPolicyService_Set_RunningSandbox_UnreachableStillFailsClosed(t *testing.T) {
	ctrl := &fakePolicyController{setErr: &model.EgressChannelUnreachableError{
		SocketPath: "/w/01SB/c.sock", VMStateSeen: true, ChannelSeen: true,
		Cause: errors.New("connect: connection refused"),
	}}
	stager := &fakePendingStager{}
	events := &recordingEvents{}
	svc := newPendingPolicyService(t, ctrl, stager, events)

	got, err := svc.Set(context.Background(), runningSandbox(), nil, false)

	require.Error(t, err)
	assert.Nil(t, got, "an unreachable enforcer is an error, never an empty policy")
	assert.Equal(t, 0, stager.stageCalls,
		"a running sandbox must never be answered from the pending launch options")
	assert.Contains(t, err.Error(), "was NOT changed")
	recorded := events.snapshot()
	require.Len(t, recorded, 2)
	assert.Contains(t, recorded[1].Detail, `"phase":"failed"`)
}

// TestEgressPolicyService_Show_RunningSandbox_UnreachableIsErrorNotEmptyPolicy
// is the same guarantee on the read path: an empty list reads as "nothing is
// allowed" when the truth may be "nothing is enforcing", and those are opposite
// facts. Refs: MGIT-72, MGIT-109, SEC-04
func TestEgressPolicyService_Show_RunningSandbox_UnreachableIsErrorNotEmptyPolicy(t *testing.T) {
	ctrl := &fakePolicyController{showErr: &model.EgressChannelUnreachableError{
		SocketPath: "/w/01SB/c.sock", VMStateSeen: true, ChannelSeen: true,
		Cause: errors.New("connect: connection refused"),
	}}
	svc := newPendingPolicyService(t, ctrl, &fakePendingStager{}, &recordingEvents{})

	got, err := svc.Show(context.Background(), runningSandbox())

	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "exited or was killed")
}

// TestEgressPolicyService_UnknownState_RoutesToLiveEnforcer verifies the
// conservative default. A state this build does not recognize must go to the
// enforcer, which fails closed — staging it would report a policy as pending
// for a sandbox that might be running under the old one.
// Refs: MGIT-109, SEC-04
func TestEgressPolicyService_UnknownState_RoutesToLiveEnforcer(t *testing.T) {
	info := createdSandbox()
	info.State = "" // unrecorded
	ctrl := &fakePolicyController{change: model.EgressPolicyChange{Entries: []string{"a:1"}}}
	stager := &fakePendingStager{}
	svc := newPendingPolicyService(t, ctrl, stager, &recordingEvents{})

	_, err := svc.Set(context.Background(), info, []string{"a:1"}, false)

	require.NoError(t, err)
	assert.Equal(t, 0, stager.stageCalls)
	assert.Equal(t, 1, ctrl.setCalls)
}

// TestNewEgressPolicyService_RequiresPendingStager keeps the new collaborator
// as non-optional as the others: a service without a pending route would fall
// back to dialing a VM that lazy provisioning has not booted, which is the
// defect being fixed. Refs: MGIT-109
func TestNewEgressPolicyService_RequiresPendingStager(t *testing.T) {
	_, err := NewEgressPolicyService(&fakePolicyController{}, nil, &recordingEvents{},
		func() time.Time { return time.Unix(0, 0).UTC() })

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "pending"),
		"the missing collaborator is named: %v", err)
}
