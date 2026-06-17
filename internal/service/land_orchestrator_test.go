package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// --- fakes ---------------------------------------------------------------

// readCloser is a bytes reader that records whether it was closed.
type readCloser struct {
	*bytes.Reader
	closed bool
}

func (r *readCloser) Close() error { r.closed = true; return nil }

// fakeLandStream serves a fixed framed object stream over the land channel.
type fakeLandStream struct {
	data    []byte
	openErr error
	rc      *readCloser
}

func (s *fakeLandStream) OpenLandStream(_ context.Context, _ string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.rc = &readCloser{Reader: bytes.NewReader(s.data)}
	return s.rc, nil
}

// fakeVerifier records attestation verifications and returns a fixed result.
type fakeVerifier struct {
	calls int
	err   error
}

func (v *fakeVerifier) Verify(_ context.Context, _ *model.Attestation) error {
	v.calls++
	return v.err
}

// fakePersister captures the verified batch and object pool handed to it.
type fakePersister struct {
	called  int
	gotTask string
	gotPool []land.Object
	landed  []land.LandedCommit
	landErr error
}

func (p *fakePersister) Land(_ context.Context, taskID string, pool []land.Object, commits []land.LandedCommit) error {
	p.called++
	p.gotTask = taskID
	p.gotPool = pool
	p.landed = commits
	return p.landErr
}

// --- helpers -------------------------------------------------------------

// frame encodes objects in the land wire format DecodeObjects reads:
// [1 type byte][4-byte BE length][payload].
func frame(objs []land.Object) []byte {
	var b bytes.Buffer
	for _, o := range objs {
		b.WriteByte(o.Type)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(o.Data)))
		b.Write(l[:])
		b.Write(o.Data)
	}
	return b.Bytes()
}

// buildCommit returns a real canonical git commit object and a model.Commit
// whose identity-bearing fields are derived from it and whose content_hash
// is self-consistent — i.e. it passes land.VerifyBinding.
func buildCommit(t *testing.T, task, msg, agent string) ([]byte, *model.Commit) {
	t.Helper()
	when := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	sig := object.Signature{Name: agent, Email: "a@x", When: when}
	gc := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: plumbing.ZeroHash}
	enc := &plumbing.MemoryObject{}
	require.NoError(t, gc.Encode(enc))
	r, err := enc.Reader()
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)

	tid, err := model.ParseTaskID(task)
	require.NoError(t, err)
	c := &model.Commit{
		CommitID:  enc.Hash().String(),
		Message:   msg,
		CreatedAt: when,
		TreeHash:  plumbing.ZeroHash.String(),
		AgentID:   agent,
		TaskID:    tid,
	}
	c.ContentHash = c.ComputeContentHash()
	return data, c
}

// attFor builds an attestation bound to the given commit and sandbox.
func attFor(c *model.Commit, sandboxID string) *model.Attestation {
	return &model.Attestation{
		SandboxID: sandboxID, CommitHash: c.CommitID, ContentHash: c.ContentHash,
		Alg: model.AlgEd25519, KeyID: "host-key",
		HostSignature: []byte("sig"), IssuedAt: c.CreatedAt,
	}
}

func policyOff() model.SandboxPolicy { return model.SandboxPolicy{RequireSandbox: false} }
func policyOn() model.SandboxPolicy  { return model.SandboxPolicy{RequireSandbox: true} }

func streamOf(data []byte) *fakeLandStream {
	return &fakeLandStream{data: frame([]land.Object{{Type: land.ObjCommit, Data: data}})}
}

func newOrch(t *testing.T, s LandStreamOpener, v AttestationVerifier, p LandPersister,
	ev SandboxEventAppender, pol SandboxPolicyReader) *LandOrchestrator {
	t.Helper()
	o, err := NewLandOrchestrator(s, v, p, ev, pol, land.DefaultLimits(),
		func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	return o
}

func landReq(task, sandbox string, ccs ...ClaimedCommit) LandRequest {
	return LandRequest{TaskID: task, SandboxID: sandbox, Commits: ccs}
}

// --- required tests ------------------------------------------------------

// TestLandOrch_HappyPath_ImportsAndFastForwards verifies a self-consistent
// commit is pulled, verified, and persisted atomically (the persister
// receives the batch and the full object pool). Refs: FR-17.5
func TestLandOrch_HappyPath_ImportsAndFastForwards(t *testing.T) {
	data, c := buildCommit(t, "MGIT-11.9.3", "feat: land me", "agent-1")
	stream := streamOf(data)
	persist := &fakePersister{}
	ev := &fakeEventAppender{}
	orch := newOrch(t, stream, &fakeVerifier{}, persist, ev, fakePolicy{p: policyOff()})

	err := orch.Land(context.Background(),
		landReq("MGIT-11.9.3", "sbx-1", ClaimedCommit{Commit: c, Attestation: attFor(c, "sbx-1")}))
	require.NoError(t, err)

	require.Equal(t, 1, persist.called, "the batch is persisted once")
	require.Len(t, persist.landed, 1)
	assert.Equal(t, "MGIT-11.9.3", persist.gotTask)
	assert.Equal(t, c.CommitID, persist.landed[0].Commit.CommitID)
	assert.NotEmpty(t, persist.gotPool, "the content-addressed object pool is passed to the persister")
	assert.True(t, stream.rc.closed, "the land stream is closed")
	assert.Equal(t, []string{model.EventLanded}, ev.types(), "a successful land is audited")
}

// TestLandOrch_VerifyFail_ImportsNothing verifies a commit whose hashes do
// not bind to its bytes is rejected and nothing is persisted or audited.
// Refs: FR-17.5, FR-17.24, SEC-06
func TestLandOrch_VerifyFail_ImportsNothing(t *testing.T) {
	data, c := buildCommit(t, "MGIT-11.9.3", "feat: land me", "agent-1")
	c.ContentHash = "0000000000000000000000000000000000000000000000000000000000000000"
	persist := &fakePersister{}
	ev := &fakeEventAppender{}
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, persist, ev, fakePolicy{p: policyOff()})

	err := orch.Land(context.Background(),
		landReq("MGIT-11.9.3", "sbx-1", ClaimedCommit{Commit: c}))
	require.ErrorIs(t, err, model.ErrLandVerificationFailed)
	assert.Zero(t, persist.called, "verification failure imports nothing")
	assert.Empty(t, ev.events, "a rejected land is not audited as landed")
}

// TestLandOrch_RecordsSandboxProvenance verifies that under require_sandbox
// a valid attestation lands the commit with its sandbox_id provenance.
// Refs: FR-17.5, FR-17.6, SEC-01
func TestLandOrch_RecordsSandboxProvenance(t *testing.T) {
	data, c := buildCommit(t, "MGIT-11.9.3", "feat: land me", "agent-1")
	persist := &fakePersister{}
	ev := &fakeEventAppender{}
	ver := &fakeVerifier{}
	orch := newOrch(t, streamOf(data), ver, persist, ev, fakePolicy{p: policyOn()})

	err := orch.Land(context.Background(),
		landReq("MGIT-11.9.3", "sbx-9", ClaimedCommit{Commit: c, Attestation: attFor(c, "sbx-9")}))
	require.NoError(t, err)
	require.Equal(t, 1, ver.calls, "the attestation is verified under require_sandbox")
	require.NotNil(t, persist.landed[0].SandboxID, "sandbox provenance is recorded")
	assert.Equal(t, "sbx-9", *persist.landed[0].SandboxID)
}

// --- error paths ---------------------------------------------------------

func TestLandOrch_StreamOpenError(t *testing.T) {
	orch := newOrch(t, &fakeLandStream{openErr: errors.New("vsock down")},
		&fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	_, c := buildCommit(t, "MGIT-1", "m", "a")
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.Error(t, err)
}

func TestLandOrch_DecodeError(t *testing.T) {
	// A frame declaring an unknown object type is a schema violation.
	bad := &fakeLandStream{data: []byte{0x99, 0, 0, 0, 0}}
	orch := newOrch(t, bad, &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	_, c := buildCommit(t, "MGIT-1", "m", "a")
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

func TestLandOrch_DuplicateObject_Rejected(t *testing.T) {
	// The same commit object served twice in one land is a schema violation
	// (CommitObjectsByID), surfaced by the orchestrator before verification.
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	dup := &fakeLandStream{data: frame([]land.Object{
		{Type: land.ObjCommit, Data: data}, {Type: land.ObjCommit, Data: data},
	})}
	persist := &fakePersister{}
	orch := newOrch(t, dup, &fakeVerifier{}, persist, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
	assert.Zero(t, persist.called)
}

func TestLandOrch_PolicyLoadError(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, errPolicy{})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.Error(t, err)
}

func TestLandOrch_MissingObjectForClaim(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	other, _ := buildCommit(t, "MGIT-1", "different", "a") // not the claimed commit's bytes
	_ = data
	orch := newOrch(t, streamOf(other), &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

func TestLandOrch_TaskMismatch_Rejected(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a") // commit is for MGIT-1
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	err := orch.Land(context.Background(), landReq("MGIT-2", "sbx", ClaimedCommit{Commit: c})) // landing into MGIT-2
	assert.ErrorIs(t, err, model.ErrTaskMismatch)
}

func TestLandOrch_RequireSandbox_NoAttestation_Rejected(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOn()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c})) // no attestation
	assert.ErrorIs(t, err, model.ErrUnattestedCommit)
}

func TestLandOrch_AttestationVerifyFails_Propagates(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	orch := newOrch(t, streamOf(data), &fakeVerifier{err: model.ErrAttestationInvalid},
		&fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOn()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c, Attestation: attFor(c, "sbx")}))
	assert.ErrorIs(t, err, model.ErrAttestationInvalid)
}

func TestLandOrch_PersistError_Surfaces(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	persist := &fakePersister{landErr: errors.New("tx failed")}
	ev := &fakeEventAppender{}
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, persist, ev, fakePolicy{p: policyOff()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.Error(t, err)
	assert.Empty(t, ev.events, "a failed persist is not audited as landed")
}

func TestLandOrch_AuditError_Surfaces(t *testing.T) {
	data, c := buildCommit(t, "MGIT-1", "m", "a")
	ev := &fakeEventAppender{failNth: 1} // the landed event fails
	orch := newOrch(t, streamOf(data), &fakeVerifier{}, &fakePersister{}, ev, fakePolicy{p: policyOff()})
	err := orch.Land(context.Background(), landReq("MGIT-1", "sbx", ClaimedCommit{Commit: c}))
	assert.Error(t, err)
}

func TestLandOrch_ValidateRequest(t *testing.T) {
	_, c := buildCommit(t, "MGIT-1", "m", "a")
	orch := newOrch(t, streamOf(nil), &fakeVerifier{}, &fakePersister{}, &fakeEventAppender{}, fakePolicy{p: policyOff()})
	for name, req := range map[string]LandRequest{
		"empty_task":    landReq("", "sbx", ClaimedCommit{Commit: c}),
		"empty_sandbox": landReq("MGIT-1", "", ClaimedCommit{Commit: c}),
		"no_commits":    landReq("MGIT-1", "sbx"),
		"nil_commit":    landReq("MGIT-1", "sbx", ClaimedCommit{Commit: nil}),
		"negative_base": {TaskID: "MGIT-1", SandboxID: "sbx", BasePosition: -1, Commits: []ClaimedCommit{{Commit: c}}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, orch.Land(context.Background(), req))
		})
	}
}

func TestNewLandOrchestrator_NilDeps(t *testing.T) {
	s := &fakeLandStream{}
	v := &fakeVerifier{}
	p := &fakePersister{}
	ev := &fakeEventAppender{}
	pol := fakePolicy{}
	clk := time.Now
	lim := land.DefaultLimits()
	for name, build := range map[string]func() (*LandOrchestrator, error){
		"nil_stream":   func() (*LandOrchestrator, error) { return NewLandOrchestrator(nil, v, p, ev, pol, lim, clk) },
		"nil_verifier": func() (*LandOrchestrator, error) { return NewLandOrchestrator(s, nil, p, ev, pol, lim, clk) },
		"nil_persist":  func() (*LandOrchestrator, error) { return NewLandOrchestrator(s, v, nil, ev, pol, lim, clk) },
		"nil_events":   func() (*LandOrchestrator, error) { return NewLandOrchestrator(s, v, p, nil, pol, lim, clk) },
		"nil_policy":   func() (*LandOrchestrator, error) { return NewLandOrchestrator(s, v, p, ev, nil, lim, clk) },
		"nil_clock":    func() (*LandOrchestrator, error) { return NewLandOrchestrator(s, v, p, ev, pol, lim, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := build()
			assert.Error(t, err)
		})
	}
}
