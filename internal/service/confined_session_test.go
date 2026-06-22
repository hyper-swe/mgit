package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeCredSink records per-session credential injections.
type fakeCredSink struct {
	calls     int
	sandboxID string
	sessionID string
	creds     []model.SessionCredential
	err       error
}

func (f *fakeCredSink) InjectCredentials(_ context.Context, sandboxID, sessionID string, creds []model.SessionCredential) error {
	f.calls++
	f.sandboxID, f.sessionID, f.creds = sandboxID, sessionID, creds
	return f.err
}

// recordingAppender captures appended audit events.
type recordingAppender struct {
	events []*model.SandboxEvent
	err    error
}

func (r *recordingAppender) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func newConfinedService(t *testing.T, sink GuestCredentialSink, app SandboxEventAppender) *ConfinedSessionService {
	t.Helper()
	n := 0
	svc, err := NewConfinedSessionService(sink, app,
		func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { n++; return "sess-" + string(rune('0'+n)), nil })
	require.NoError(t, err)
	return svc
}

// TestT2_ConfineAgent_OptInOnly verifies a confined session is refused when
// confine_agent is off (the default), and nothing is injected or audited.
// Refs: MGIT-11.11.4
func TestT2_ConfineAgent_OptInOnly(t *testing.T) {
	sink := &fakeCredSink{}
	app := &recordingAppender{}
	svc := newConfinedService(t, sink, app)

	_, err := svc.Start(context.Background(), "sbx-1", "MGIT-4.2", false,
		[]model.SessionCredential{{Name: "ANTHROPIC_API_KEY", Value: "sk-secret"}})

	require.ErrorIs(t, err, model.ErrConfineAgentDisabled)
	assert.Zero(t, sink.calls, "no credential injection when T2 is off")
	assert.Empty(t, app.events, "no audit event when T2 is off")
}

// TestT2_CredentialsInjectedPerSession_Audited verifies credentials are
// injected for a fresh session ID and the injection is flagged in the audit
// log by credential NAME only — never the secret value. Refs: MGIT-11.11.4
func TestT2_CredentialsInjectedPerSession_Audited(t *testing.T) {
	sink := &fakeCredSink{}
	app := &recordingAppender{}
	svc := newConfinedService(t, sink, app)

	creds := []model.SessionCredential{{Name: "ANTHROPIC_API_KEY", Value: "sk-secret-xyz"}}
	sess, err := svc.Start(context.Background(), "sbx-1", "MGIT-4.2", true, creds)
	require.NoError(t, err)

	// Injected once, per-session, with the secret carried to the guest.
	require.Equal(t, 1, sink.calls)
	assert.Equal(t, sess, sink.sessionID, "credentials scoped to this session")
	assert.Equal(t, "sbx-1", sink.sandboxID)
	assert.Equal(t, creds, sink.creds)

	// Audited as credentials_injected, flagged by name, secret absent.
	require.Len(t, app.events, 1)
	ev := app.events[0]
	assert.Equal(t, model.EventCredentialsInjected, ev.EventType)
	assert.Equal(t, "sbx-1", ev.SandboxID)
	assert.Contains(t, ev.Detail, "ANTHROPIC_API_KEY", "name flagged")
	assert.NotContains(t, ev.Detail, "sk-secret-xyz", "secret value never audited")
}

// TestT2_NoCredentialsInImage verifies credentials never travel via the
// launch/image config: the launch options carry no secret, and a confined
// launch serializes without any credential value. Credentials flow ONLY
// through the per-session Start path. Refs: MGIT-11.11.4
func TestT2_NoCredentialsInImage(t *testing.T) {
	opts := model.SandboxLaunchOptions{
		TaskID: "MGIT-4.2", WorktreePath: "/wt",
		ImageRef: "agent-img@sha256:" + repeat64('a'), ConfineAgent: true,
	}
	b, err := json.Marshal(opts)
	require.NoError(t, err)
	assert.Contains(t, string(b), "confine_agent", "T2 flag is on the image config")
	assert.NotContains(t, string(b), "sk-", "no credential material in the launch/image config")

	// The audit Detail likewise never carries values (names only).
	detail, err := credentialAuditDetail("sess-1",
		[]model.SessionCredential{{Name: "GH_TOKEN", Value: "ghp_secret"}})
	require.NoError(t, err)
	assert.NotContains(t, detail, "ghp_secret")
}

// TestConfinedSession_InjectionFailure_NotAudited verifies a failed
// injection aborts the session and records no injection event. Refs: MGIT-11.11.4
func TestConfinedSession_InjectionFailure_NotAudited(t *testing.T) {
	sink := &fakeCredSink{err: errors.New("guest channel closed")}
	app := &recordingAppender{}
	svc := newConfinedService(t, sink, app)

	_, err := svc.Start(context.Background(), "sbx-1", "MGIT-4.2", true, nil)
	require.Error(t, err)
	assert.Empty(t, app.events, "no injection audit when injection failed")
}

// TestConfinedSession_AuditFailure_Surfaced verifies an audit-append
// failure aborts the session. Refs: MGIT-11.11.4
func TestConfinedSession_AuditFailure_Surfaced(t *testing.T) {
	sink := &fakeCredSink{}
	app := &recordingAppender{err: errors.New("audit store down")}
	svc := newConfinedService(t, sink, app)

	_, err := svc.Start(context.Background(), "sbx-1", "MGIT-4.2", true, nil)
	require.Error(t, err)
}

// TestConfinedSession_EmptySandboxID rejects an empty sandbox id. Refs: MGIT-11.11.4
func TestConfinedSession_EmptySandboxID(t *testing.T) {
	svc := newConfinedService(t, &fakeCredSink{}, &recordingAppender{})
	_, err := svc.Start(context.Background(), "", "MGIT-4.2", true, nil)
	assert.Error(t, err)
}

// TestConfinedSession_NewIDError surfaces a session-id generation failure
// before any injection. Refs: MGIT-11.11.4
func TestConfinedSession_NewIDError(t *testing.T) {
	sink := &fakeCredSink{}
	svc, err := NewConfinedSessionService(sink, &recordingAppender{},
		func() time.Time { return time.Unix(0, 0).UTC() },
		func() (string, error) { return "", errors.New("entropy exhausted") })
	require.NoError(t, err)

	_, err = svc.Start(context.Background(), "sbx-1", "MGIT-4.2", true, nil)
	require.Error(t, err)
	assert.Zero(t, sink.calls, "no injection when the session id could not be minted")
}

// TestNewConfinedSessionService_NilDeps rejects nil dependencies. Refs: MGIT-11.11.4
func TestNewConfinedSessionService_NilDeps(t *testing.T) {
	_, err := NewConfinedSessionService(nil, &recordingAppender{}, nil, nil)
	assert.Error(t, err)
}

// repeat64 builds a 64-char hex-ish digest body for a test image ref.
func repeat64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
