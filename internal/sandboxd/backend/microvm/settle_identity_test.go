package microvm

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// recordingGuestExec is a settle exec seam that records every request the
// settler sends and answers with a chosen identity echo. For a request that
// carries paths it recognizes, it emits sha256sum-shaped lines with matching
// digests, so a well-behaved probe settles clean. Refs: MGIT-272
type recordingGuestExec struct {
	mu       sync.Mutex
	requests []model.ExecRequest
	ranAs    *model.GuestIdentity // echoed on every result; nil = the guest did not say
	hashes   map[string]string    // absolute path -> digest to emit
}

func (r *recordingGuestExec) run(_ context.Context, _ string, req model.ExecRequest) (*model.ExecResult, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	var out strings.Builder
	for _, a := range req.Command {
		if h, ok := r.hashes[a]; ok {
			fmt.Fprintf(&out, "%s  %s\n", h, a)
		}
	}
	return &model.ExecResult{Stdout: []byte(out.String()), ExitCode: 0, RanAs: r.ranAs}, nil
}

func (r *recordingGuestExec) sent() []model.ExecRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.ExecRequest(nil), r.requests...)
}

// recordingAuditor captures the privileged-internal-exec events the settler
// appends. Refs: MGIT-272
type recordingAuditor struct {
	mu     sync.Mutex
	events []*model.SandboxEvent
}

func (r *recordingAuditor) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingAuditor) recorded() []*model.SandboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*model.SandboxEvent(nil), r.events...)
}

// oneFileSettle wires an execSettler over a recording exec for a single-file
// manifest whose digest the exec echoes back, with the given identity echo.
func oneFileSettle(ranAs *model.GuestIdentity, internal *model.GuestIdentity, aud model.SandboxEventAppender,
) (execSettler, *recordingGuestExec, settleRequest) {
	const wt = "/wt"
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	abs := path.Join(wt, "app.go")
	rec := &recordingGuestExec{ranAs: ranAs, hashes: map[string]string{abs: digest}}
	m := &Manager{internalIdentity: internal, internalAudit: aud}
	s := execSettler{m: m, exec: rec.run}
	req := settleRequest{
		sandboxID: "sb1", taskID: "MGIT-272", network: model.NetworkModeNone, worktree: wt,
		want: worktreesync.Manifest{"app.go": {Hash: digest, Mode: 0o644}},
	}
	return s, rec, req
}

// A settle exec, when the internal identity is wired, runs as that identity
// (never as an implicit default) — the requests the settler sends carry it.
// Refs: MGIT-272, MGIT-151
func TestSettleProbe_WhenWired_RunsAsTheInternalIdentity(t *testing.T) {
	id := model.RootIdentity()
	s, rec, req := oneFileSettle(&id, &id, &recordingAuditor{})

	view, err := s.Probe(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, view.stale, "the manifest digest matches, so nothing is stale")

	sent := rec.sent()
	require.NotEmpty(t, sent, "the settler asked the guest at least once")
	for _, rq := range sent {
		require.NotNilf(t, rq.RunAs, "every settle exec carries the internal identity: %v", rq.Command)
		assert.Equal(t, id, *rq.RunAs)
	}
}

// Every program the settler asks the guest to run is named by an absolute
// path, so it cannot be resolved to something placed on the guest's search
// path. Refs: MGIT-272
func TestSettleProbe_NamesEveryProgramByAbsolutePath(t *testing.T) {
	id := model.RootIdentity()
	s, rec, req := oneFileSettle(&id, &id, &recordingAuditor{})
	req.deleted = []string{"gone.go"} // exercise the presence-check exec too

	_, err := s.Probe(context.Background(), req)
	require.NoError(t, err)

	sent := rec.sent()
	require.NotEmpty(t, sent)
	for _, rq := range sent {
		require.NotEmpty(t, rq.Command)
		assert.Truef(t, path.IsAbs(rq.Command[0]),
			"every settle exec names its program by an absolute path: %q", rq.Command[0])
	}
}

// A guest that confirms it ran the settle exec as a DIFFERENT identity than
// asked is not trusted: the sync fails closed. Refs: MGIT-272, MGIT-151
func TestSettleProbe_WhenGuestRanAsUnexpectedIdentity_FailsClosed(t *testing.T) {
	internal := model.RootIdentity()
	wrong := model.IdentityForProcess(12345, 6789)
	s, _, req := oneFileSettle(&wrong, &internal, &recordingAuditor{})

	_, err := s.Probe(context.Background(), req)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrGuestExecIdentityMismatch)
}

// A guest that does not report the identity it ran as (a base predating the
// field) is unverifiable — a soft "cannot tell", never a hard failure.
// Refs: MGIT-272, MGIT-174
func TestSettleProbe_WhenGuestDidNotConfirmIdentity_StaysUnverifiedNotFailed(t *testing.T) {
	internal := model.RootIdentity()
	s, _, req := oneFileSettle(nil, &internal, &recordingAuditor{}) // nil echo = old base

	view, err := s.Probe(context.Background(), req)
	require.NoError(t, err, "an unconfirmed identity is soft, not a hard failure")
	assert.NotEmpty(t, view.unverifiable, "the guest did not confirm the identity it ran as")
	assert.Empty(t, view.stale)
}

// The content digest stays a HARD gate even when the identity is unconfirmed:
// an old base that reads the wrong bytes is still a refusal, never masked by
// the soft identity note. Refs: MGIT-272
func TestSettleProbe_WhenIdentityUnconfirmedButContentStale_ContentStillFailsHard(t *testing.T) {
	internal := model.RootIdentity()
	s, rec, req := oneFileSettle(nil, &internal, &recordingAuditor{})
	for k := range rec.hashes { // the guest reads a digest that was never staged
		rec.hashes[k] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}

	view, err := s.Probe(context.Background(), req)
	require.NoError(t, err)
	assert.NotEmpty(t, view.stale, "a content mismatch is reported even when the identity is unconfirmed")
	assert.Empty(t, view.unverifiable, "an unconfirmed identity must not mask a content mismatch")
}

// A current base confirms the identity it ran as, so the soft cannot-tell
// path is not reached — and no file content the guest reads can select it,
// because the confirmation comes from the exec result, not the tree.
// Refs: MGIT-272
func TestSettleProbe_WhenGuestConfirmsIdentity_IsNotMarkedUnverified(t *testing.T) {
	internal := model.RootIdentity()
	s, _, req := oneFileSettle(&internal, &internal, &recordingAuditor{})

	view, err := s.Probe(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, view.unverifiable, "a confirmed identity is not the soft cannot-tell path")
	assert.Empty(t, view.stale)
}

// Each settle probe is recorded once as a privileged internal exec, with the
// task and sandbox it ran for. Refs: MGIT-272, FR-17.18
func TestSettleProbe_WhenWired_IsRecordedOnce(t *testing.T) {
	id := model.RootIdentity()
	aud := &recordingAuditor{}
	s, _, req := oneFileSettle(&id, &id, aud)

	_, err := s.Probe(context.Background(), req)
	require.NoError(t, err)

	events := aud.recorded()
	require.Len(t, events, 1, "one privileged-internal-exec record per probe")
	assert.Equal(t, model.EventExecPrivileged, events[0].EventType)
	assert.Equal(t, "sb1", events[0].SandboxID)
	assert.Equal(t, "MGIT-272", events[0].TaskID)
	assert.NotEmpty(t, events[0].Detail, "the record names the program it ran")
}

// A guest whose settle exec cannot even start — no usable shell inside, so the
// program the probe names cannot be run — is a soft "cannot tell", never a hard
// failure. On such a guest the read-back was never verifiable from inside (the
// content digest stays the real verdict on a guest that can run it). Refs: MGIT-272, MGIT-192
func TestSettleProbe_WhenTheReadBackCannotStart_IsUnverifiedNotFailed(t *testing.T) {
	id := model.RootIdentity()
	m := &Manager{internalIdentity: &id, internalAudit: &recordingAuditor{}}
	cannotStart := func(_ context.Context, _ string, _ model.ExecRequest) (*model.ExecResult, error) {
		return nil, fmt.Errorf(`libkrun exec: guest exec: start "/bin/sh": fork/exec /bin/sh: no such file or directory`)
	}
	s := execSettler{m: m, exec: cannotStart}
	req := settleRequest{
		sandboxID: "sb1", taskID: "MGIT-272", network: model.NetworkModeNone, worktree: "/wt",
		want: worktreesync.Manifest{"app.go": {Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: 0o644}},
	}
	view, err := s.Probe(context.Background(), req)
	require.NoError(t, err, "a guest whose read-back cannot start is unverifiable, not a hard error")
	assert.NotEmpty(t, view.unverifiable, "the guest could not verify from inside — a loud 'cannot tell'")
	assert.Empty(t, view.stale, "an exec that could not start reports nothing stale")
}
