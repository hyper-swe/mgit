package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The identity a guest exec runs as is the daemon's decision: the daemon
// delivered the tree and the base as itself, so that is the uid/gid a
// command can read and write as. The service fills it on every exec.
// Refs: MGIT-151
func TestExec_FillsTheDaemonIdentity(t *testing.T) {
	agent := model.GuestIdentity{UID: 501, GID: 20, Name: "agent", Home: "/home/agent"}
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{RanAs: &agent}}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	svc.SetExecIdentity(agent)
	_, err := svc.Register(context.Background(), regOpts("MGIT-151", "/work/a"))
	require.NoError(t, err)
	res, err := svc.Exec(context.Background(), "MGIT-151", model.ExecRequest{Command: []string{"id"}})
	require.NoError(t, err)
	require.NotNil(t, mgr.lastExecReq.RunAs, "the request carries the identity to the guest")
	assert.Equal(t, agent, *mgr.lastExecReq.RunAs)
	assert.False(t, mgr.lastExecReq.AsRoot)
	require.NotNil(t, res.Identity, "every exec gets a verdict")
	assert.True(t, res.Identity.Verified, "the guest echoed what was asked")
}

// `--as-root` is the only way to root: the service audits it as its own
// event and asks the guest for root explicitly. Refs: MGIT-151, FR-17.18
func TestExec_AsRoot_AuditsAndAsksForRoot(t *testing.T) {
	agent := model.GuestIdentity{UID: 501, GID: 20}
	root := model.RootIdentity()
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{RanAs: &root}}
	events := &fakeEventAppender{}
	svc := newSvc(t, mgr, events)
	svc.SetExecIdentity(agent)
	reg, err := svc.Register(context.Background(), regOpts("MGIT-151", "/work/a"))
	require.NoError(t, err)
	res, err := svc.Exec(context.Background(), "MGIT-151",
		model.ExecRequest{Command: []string{"apt-get", "install", "-y", "curl"}, AsRoot: true})
	require.NoError(t, err)
	require.NotNil(t, mgr.lastExecReq.RunAs)
	assert.True(t, mgr.lastExecReq.RunAs.IsRoot(), "the guest is asked for root explicitly")
	assert.True(t, mgr.lastExecReq.AsRoot)
	require.NotNil(t, res.Identity)
	assert.True(t, res.Identity.Verified)
	assert.Contains(t, events.types(), model.EventExecPrivileged, "the escalation is an audit event of its own")
	var ev model.SandboxEvent
	for _, e := range events.events {
		if e.EventType == model.EventExecPrivileged {
			ev = e
		}
	}
	assert.Equal(t, reg.ID, ev.SandboxID)
	assert.Equal(t, "MGIT-151", ev.TaskID)
	var detail struct {
		Program string `json:"program"`
		Args    int    `json:"args"`
	}
	require.NoError(t, json.Unmarshal([]byte(ev.Detail), &detail))
	assert.Equal(t, "apt-get", detail.Program, "the audit names the program")
	assert.Equal(t, 3, detail.Args, "and counts its arguments; it never records them (they may carry secrets)")
}

// A client cannot choose an identity: run_as is the daemon's field, and a
// request that arrives with one set is refused — the only road to root is
// as_root, which is audited. Refs: MGIT-151, SEC-05
func TestExec_ClientSuppliedRunAs_IsRefused(t *testing.T) {
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{}}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	svc.SetExecIdentity(model.GuestIdentity{UID: 501, GID: 20})
	_, err := svc.Register(context.Background(), regOpts("MGIT-151", "/work/a"))
	require.NoError(t, err)
	root := model.RootIdentity()
	_, err = svc.Exec(context.Background(), "MGIT-151", model.ExecRequest{Command: []string{"id"}, RunAs: &root})
	require.Error(t, err)
	assert.ErrorContains(t, err, "run_as")
	assert.ErrorContains(t, err, "--as-root")
	assert.Equal(t, 0, mgr.execs, "nothing ran")
}

// The verdict is loud when the guest cannot confirm: a guest that predates
// the field (RanAs nil) or that ran as something else. Refs: MGIT-151
func TestExec_VerdictWhenTheGuestCannotConfirm(t *testing.T) {
	agent := model.GuestIdentity{UID: 501, GID: 20}
	root := model.RootIdentity()
	tests := []struct {
		name       string
		ranAs      *model.GuestIdentity
		wantReason string
	}{
		{name: "old_guest_says_nothing", ranAs: nil, wantReason: "did not report"},
		{name: "guest_ran_as_root", ranAs: &root, wantReason: "ran as uid 0 gid 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &fakeSandboxManager{execResult: &model.ExecResult{ExitCode: 0, RanAs: tt.ranAs}}
			svc := newSvc(t, mgr, &fakeEventAppender{})
			svc.SetExecIdentity(agent)
			_, err := svc.Register(context.Background(), regOpts("MGIT-151", "/work/a"))
			require.NoError(t, err)
			res, err := svc.Exec(context.Background(), "MGIT-151", model.ExecRequest{Command: []string{"id"}})
			require.NoError(t, err, "the command ran; the verdict rides the result, it does not fail it")
			require.NotNil(t, res.Identity)
			assert.False(t, res.Identity.Verified)
			assert.Contains(t, res.Identity.Reason, tt.wantReason)
		})
	}
}

// With no identity configured (memory-only wiring, tests) the service asks
// for nothing and says so in the verdict, rather than inventing one.
// Refs: MGIT-151
func TestExec_NoIdentityConfigured_AsksNothingAndSaysSo(t *testing.T) {
	mgr := &fakeSandboxManager{execResult: &model.ExecResult{}}
	svc := newSvc(t, mgr, &fakeEventAppender{})
	_, err := svc.Register(context.Background(), regOpts("MGIT-151", "/work/a"))
	require.NoError(t, err)
	res, err := svc.Exec(context.Background(), "MGIT-151", model.ExecRequest{Command: []string{"id"}})
	require.NoError(t, err)
	assert.Nil(t, mgr.lastExecReq.RunAs)
	require.NotNil(t, res.Identity)
	assert.False(t, res.Identity.Verified)
	assert.Contains(t, res.Identity.Reason, "no identity was asked for")
}
