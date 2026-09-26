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
// carries paths it recognises, it emits sha256sum-shaped lines with matching
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
