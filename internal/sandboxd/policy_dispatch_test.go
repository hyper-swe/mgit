package sandboxd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// fakePolicyCoordinator records the sandbox binding the dispatch resolved and
// returns canned outcomes.
type fakePolicyCoordinator struct {
	gotInfo  model.SandboxInfo
	gotEntry []string
	gotDrain bool
	change   *model.EgressPolicyChange
	state    *model.EgressPolicyState
	err      error
}

func (f *fakePolicyCoordinator) Set(
	_ context.Context, info model.SandboxInfo, entries []string, drain bool,
) (*model.EgressPolicyChange, error) {
	f.gotInfo, f.gotEntry, f.gotDrain = info, entries, drain
	if f.err != nil {
		return nil, f.err
	}
	return f.change, nil
}

func (f *fakePolicyCoordinator) Show(
	_ context.Context, info model.SandboxInfo,
) (*model.EgressPolicyState, error) {
	f.gotInfo = info
	if f.err != nil {
		return nil, f.err
	}
	return f.state, nil
}

// policyRoundTrip runs one control request against a daemon wired with pc
// (nil leaves the verb unwired) and returns the reply.
func policyRoundTrip(t *testing.T, pc PolicyCoordinator, svcErr error, req *controlproto.Request) *controlproto.Response {
	t.Helper()
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{opErr: svcErr})
	cfg.Policy = pc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, req))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)

	cancel()
	require.NoError(t, <-done)
	return resp
}

// TestDaemon_PolicySetKind_NotServedWhenUnwired verifies the verb reports
// itself unserved rather than silently succeeding when no enforcer is wired.
//
// A "revoke succeeded" from a daemon with nothing to revoke through is the
// most dangerous possible reply: the caller then runs untrusted code believing
// egress is closed. Refs: MGIT-72, SEC-04
func TestDaemon_PolicySetKind_NotServedWhenUnwired(t *testing.T) {
	resp := policyRoundTrip(t, nil, nil, &controlproto.Request{
		Kind:      controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{TaskID: "MGIT-72"},
	})

	assert.NotEmpty(t, resp.Error, "policy set is not served without a coordinator")
	assert.Nil(t, resp.Policy)
}

// TestDaemon_PolicyShowKind_NotServedWhenUnwired is the same guard on the read
// verb: an unwired daemon must not answer with an empty policy, which would
// read as "nothing is allowed" when the truth is "nothing is enforcing".
func TestDaemon_PolicyShowKind_NotServedWhenUnwired(t *testing.T) {
	resp := policyRoundTrip(t, nil, nil, &controlproto.Request{
		Kind:       controlproto.KindPolicyShow,
		PolicyShow: &controlproto.TaskRef{TaskID: "MGIT-72"},
	})

	assert.NotEmpty(t, resp.Error)
	assert.Nil(t, resp.Policy)
}

// TestDaemon_PolicySet_ResolvesTaskAndApplies verifies the daemon resolves
// task->sandbox through the SERVICE (host-anchored, never client-asserted) and
// passes the resolved binding on, replying with the outcome.
// Refs: MGIT-72, SEC-05
func TestDaemon_PolicySet_ResolvesTaskAndApplies(t *testing.T) {
	pc := &fakePolicyCoordinator{change: &model.EgressPolicyChange{
		Entries: []string{"registry.npmjs.org:443"}, RuleCount: 1, Killed: 3,
	}}

	resp := policyRoundTrip(t, pc, nil, &controlproto.Request{
		Kind: controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{
			TaskID: "MGIT-72", Entries: []string{"registry.npmjs.org:443"},
		},
	})

	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Policy)
	assert.Equal(t, "01JXSBSANDBOX", pc.gotInfo.ID,
		"the sandbox must come from the service's task resolution, not from the request")
	assert.Equal(t, []string{"registry.npmjs.org:443"}, pc.gotEntry)
	assert.False(t, pc.gotDrain, "kill is the default; drain must be asked for by name")
	assert.Equal(t, 3, resp.Policy.Killed)
	assert.Equal(t, 1, resp.Policy.RuleCount)
}

// TestDaemon_PolicySet_DrainIsCarried is the matching positive control for the
// default above: the opt-in weaker behavior reaches the service intact and is
// reported back, so a caller can tell which one they got.
func TestDaemon_PolicySet_DrainIsCarried(t *testing.T) {
	pc := &fakePolicyCoordinator{change: &model.EgressPolicyChange{Drained: true}}

	resp := policyRoundTrip(t, pc, nil, &controlproto.Request{
		Kind:      controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{TaskID: "MGIT-72", Drain: true},
	})

	require.Empty(t, resp.Error)
	assert.True(t, pc.gotDrain)
	require.NotNil(t, resp.Policy)
	assert.True(t, resp.Policy.Drained)
}

// TestDaemon_PolicySet_ServiceError_IsReported verifies a refusal from the
// enforcer surfaces as an error, never as an empty success.
func TestDaemon_PolicySet_ServiceError_IsReported(t *testing.T) {
	pc := &fakePolicyCoordinator{err: errors.New("vm control channel unreachable")}

	resp := policyRoundTrip(t, pc, nil, &controlproto.Request{
		Kind:      controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{TaskID: "MGIT-72"},
	})

	assert.Contains(t, resp.Error, "unreachable")
	assert.Nil(t, resp.Policy)
}

// TestDaemon_PolicySet_UnknownTask_IsReported verifies an unresolvable task is
// an error rather than a mutation applied to nothing.
func TestDaemon_PolicySet_UnknownTask_IsReported(t *testing.T) {
	pc := &fakePolicyCoordinator{}

	resp := policyRoundTrip(t, pc, errors.New("sandbox not found"), &controlproto.Request{
		Kind:      controlproto.KindPolicySet,
		PolicySet: &controlproto.PolicyArgs{TaskID: "MGIT-999"},
	})

	assert.Contains(t, resp.Error, "not found")
	assert.Nil(t, resp.Policy)
	assert.Empty(t, pc.gotInfo.ID, "no policy call may be made for a task that does not resolve")
}

// TestDaemon_PolicyShow_ReportsTheLivePolicy verifies the read verb returns
// what is enforced now, so a caller can confirm a revoke took effect rather
// than taking it on faith. Refs: MGIT-72
func TestDaemon_PolicyShow_ReportsTheLivePolicy(t *testing.T) {
	pc := &fakePolicyCoordinator{state: &model.EgressPolicyState{
		Entries: []string{"a.example:443"}, RuleCount: 1,
	}}

	resp := policyRoundTrip(t, pc, nil, &controlproto.Request{
		Kind:       controlproto.KindPolicyShow,
		PolicyShow: &controlproto.TaskRef{TaskID: "MGIT-72"},
	})

	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Policy)
	assert.Equal(t, []string{"a.example:443"}, resp.Policy.Entries)
	assert.Equal(t, "01JXSBSANDBOX", pc.gotInfo.ID)
}
