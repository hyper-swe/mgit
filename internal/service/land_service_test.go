package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// --- object pool builder -------------------------------------------------

type poolBuilder struct {
	t  *testing.T
	st *memory.Storage
}

func newPoolBuilder(t *testing.T) *poolBuilder {
	t.Helper()
	return &poolBuilder{t: t, st: memory.NewStorage()}
}

func (b *poolBuilder) raw(h plumbing.Hash) []byte {
	b.t.Helper()
	o, err := b.st.EncodedObject(plumbing.AnyObject, h)
	require.NoError(b.t, err)
	r, err := o.Reader()
	require.NoError(b.t, err)
	data, err := io.ReadAll(r)
	require.NoError(b.t, err)
	return data
}

func (b *poolBuilder) blob(content string) plumbing.Hash {
	b.t.Helper()
	o := b.st.NewEncodedObject()
	o.SetType(plumbing.BlobObject)
	w, _ := o.Writer()
	_, _ = w.Write([]byte(content))
	_ = w.Close()
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *poolBuilder) tree(entries ...object.TreeEntry) plumbing.Hash {
	b.t.Helper()
	o := b.st.NewEncodedObject()
	require.NoError(b.t, (&object.Tree{Entries: entries}).Encode(o))
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

func (b *poolBuilder) commit(msg string, tree, parent plumbing.Hash) plumbing.Hash {
	b.t.Helper()
	sig := object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()}
	gc := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree}
	if !parent.IsZero() {
		gc.ParentHashes = []plumbing.Hash{parent}
	}
	o := b.st.NewEncodedObject()
	require.NoError(b.t, gc.Encode(o))
	h, err := b.st.SetEncodedObject(o)
	require.NoError(b.t, err)
	return h
}

// singleCommitPool builds a one-commit pool (blob+tree+commit) and returns
// the pool and the commit hash.
func singleCommitPool(b *poolBuilder, msg, file, content string) ([]land.Object, plumbing.Hash) {
	blob := b.blob(content)
	tree := b.tree(object.TreeEntry{Name: file, Mode: filemode.Regular, Hash: blob})
	commit := b.commit(msg, tree, plumbing.ZeroHash)
	return []land.Object{
		{Type: land.ObjBlob, Data: b.raw(blob)},
		{Type: land.ObjTree, Data: b.raw(tree)},
		{Type: land.ObjCommit, Data: b.raw(commit)},
	}, commit
}

// --- fakes ---------------------------------------------------------------

type fakeResolver struct {
	info *model.SandboxInfo
	err  error
}

func (f *fakeResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return f.info, f.err
}

type fakeLandPuller struct {
	pool      []land.Object
	pullErr   error
	pulls     int
	discarded int
}

func (f *fakeLandPuller) Pull(context.Context, string) ([]land.Object, error) {
	f.pulls++
	return f.pool, f.pullErr
}
func (f *fakeLandPuller) Discard(string) { f.discarded++ }

type fakeLedger struct {
	commits []index.CommitRecord
	err     error
}

func (f *fakeLedger) GetTaskCommits(context.Context, string) ([]index.CommitRecord, error) {
	return f.commits, f.err
}

type fakeAttestor struct{ calls int }

func (f *fakeAttestor) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	f.calls++
	return &model.Attestation{
		SandboxID: sandboxID, CommitHash: commitHash, ContentHash: contentHash,
		Alg: model.AlgEd25519, KeyID: "host-key", HostSignature: []byte("sig"),
		IssuedAt: time.Unix(0, 0).UTC(),
	}, nil
}

// fakeParents resolves an empty parent set (initial commits) and records the
// register/deregister lifecycle.
type fakeParents struct {
	registered int
	dereg      int
}

func (f *fakeParents) ParentFileSet(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f *fakeParents) Register(pool []land.Object) ([]string, error) {
	f.registered++
	return []string{"id"}, nil
}
func (f *fakeParents) Deregister([]string) { f.dereg++ }

type fakeOrchestrator struct {
	called int
	req    LandRequest
	err    error
}

func (f *fakeOrchestrator) Land(_ context.Context, req LandRequest) error {
	f.called++
	f.req = req
	return f.err
}

type landSvcFakes struct {
	resolver *fakeResolver
	puller   *fakeLandPuller
	ledger   *fakeLedger
	parents  *fakeParents
	attestor *fakeAttestor
	orch     *fakeOrchestrator
	policy   SandboxPolicyReader
}

func newLandSvc(t *testing.T, f landSvcFakes) *LandService {
	t.Helper()
	svc, err := NewLandService(f.resolver, f.puller, f.ledger, f.parents, f.attestor,
		f.orch, f.policy, func() time.Time { return time.Unix(0, 0).UTC() })
	require.NoError(t, err)
	return svc
}

func defaultFakes(pool []land.Object, policy SandboxPolicyReader) landSvcFakes {
	return landSvcFakes{
		resolver: &fakeResolver{info: &model.SandboxInfo{ID: "sbx-1", TaskID: "MGIT-11.10.10"}},
		puller:   &fakeLandPuller{pool: pool},
		ledger:   &fakeLedger{},
		parents:  &fakeParents{},
		attestor: &fakeAttestor{},
		orch:     &fakeOrchestrator{},
		policy:   policy,
	}
}

// --- tests ---------------------------------------------------------------

// TestLandService_HappyPath_RoutesDerivedBatch verifies the service resolves
// the sandbox, derives the new commit, and routes it through the orchestrator
// with the host-anchored sandbox id and base position. Refs: FR-17.5
func TestLandService_HappyPath_RoutesDerivedBatch(t *testing.T) {
	b := newPoolBuilder(t)
	pool, commit := singleCommitPool(b, "feat: land", "a.txt", "hello")
	f := defaultFakes(pool, fakePolicy{p: policyOff()})
	svc := newLandSvc(t, f)

	sum, err := svc.Land(context.Background(), "MGIT-11.10.10")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Commits)
	assert.Equal(t, "task/MGIT-11.10.10", sum.Branch)

	require.Equal(t, 1, f.orch.called, "the batch routes through the orchestrator")
	require.Len(t, f.orch.req.Commits, 1)
	assert.Equal(t, "MGIT-11.10.10", f.orch.req.TaskID)
	assert.Equal(t, "sbx-1", f.orch.req.SandboxID)
	assert.Equal(t, 0, f.orch.req.BasePosition)
	assert.Equal(t, commit.String(), f.orch.req.Commits[0].Commit.CommitID)
	assert.Nil(t, f.orch.req.Commits[0].Attestation, "policy off issues no attestation")
	assert.Equal(t, 1, f.parents.registered)
	assert.Equal(t, 1, f.parents.dereg, "the pool is deregistered after the land")
}

// TestLandService_RequireSandbox_IssuesAttestation verifies a host
// attestation is issued and attached under require_sandbox. Refs: FR-17.6, SEC-01
func TestLandService_RequireSandbox_IssuesAttestation(t *testing.T) {
	b := newPoolBuilder(t)
	pool, commit := singleCommitPool(b, "feat: land", "a.txt", "hello")
	f := defaultFakes(pool, fakePolicy{p: policyOn()})
	svc := newLandSvc(t, f)

	_, err := svc.Land(context.Background(), "MGIT-11.10.10")
	require.NoError(t, err)
	require.Equal(t, 1, f.attestor.calls, "an attestation is issued under require_sandbox")
	att := f.orch.req.Commits[0].Attestation
	require.NotNil(t, att)
	assert.Equal(t, "sbx-1", att.SandboxID)
	assert.Equal(t, commit.String(), att.CommitHash, "the attestation binds the host-computed commit hash")
}

// TestLandService_BasePositionFromLedger verifies the base position is the
// count of already-landed commits and base history is excluded. Refs: FR-17.5
func TestLandService_BasePositionFromLedger(t *testing.T) {
	b := newPoolBuilder(t)
	// Base commit already landed; one new child.
	blob := b.blob("v1")
	tree := b.tree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blob})
	base := b.commit("base", tree, plumbing.ZeroHash)
	blob2 := b.blob("v2")
	tree2 := b.tree(object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: blob2})
	child := b.commit("edit", tree2, base)
	pool := []land.Object{
		{Type: land.ObjBlob, Data: b.raw(blob)}, {Type: land.ObjTree, Data: b.raw(tree)},
		{Type: land.ObjCommit, Data: b.raw(base)},
		{Type: land.ObjBlob, Data: b.raw(blob2)}, {Type: land.ObjTree, Data: b.raw(tree2)},
		{Type: land.ObjCommit, Data: b.raw(child)},
	}
	f := defaultFakes(pool, fakePolicy{p: policyOff()})
	f.ledger.commits = []index.CommitRecord{{CommitHash: base.String(), Position: 0}}
	svc := newLandSvc(t, f)

	sum, err := svc.Land(context.Background(), "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Commits, "only the new child lands")
	require.Len(t, f.orch.req.Commits, 1)
	assert.Equal(t, child.String(), f.orch.req.Commits[0].Commit.CommitID)
	assert.Equal(t, 1, f.orch.req.BasePosition, "base position follows the landed count")
}

// TestLandService_NothingNew_NoOp verifies a pool with only already-landed
// commits is a success no-op that never calls the orchestrator and drops the
// buffer. Refs: FR-17.5
func TestLandService_NothingNew_NoOp(t *testing.T) {
	b := newPoolBuilder(t)
	pool, commit := singleCommitPool(b, "feat: land", "a.txt", "hello")
	f := defaultFakes(pool, fakePolicy{p: policyOff()})
	f.ledger.commits = []index.CommitRecord{{CommitHash: commit.String()}}
	svc := newLandSvc(t, f)

	sum, err := svc.Land(context.Background(), "MGIT-1")
	require.NoError(t, err)
	assert.Equal(t, 0, sum.Commits)
	assert.Zero(t, f.orch.called, "nothing is routed when there is nothing new")
	assert.Equal(t, 1, f.puller.discarded, "the buffered pool is dropped")
}

func TestLandService_PullError_Surfaces(t *testing.T) {
	f := defaultFakes(nil, fakePolicy{p: policyOff()})
	f.puller.pullErr = errors.New("vsock down")
	svc := newLandSvc(t, f)
	_, err := svc.Land(context.Background(), "MGIT-1")
	assert.Error(t, err)
	assert.Zero(t, f.orch.called)
}

func TestLandService_OrchError_DiscardsAndPropagates(t *testing.T) {
	b := newPoolBuilder(t)
	pool, _ := singleCommitPool(b, "feat: land", "a.txt", "hello")
	f := defaultFakes(pool, fakePolicy{p: policyOff()})
	f.orch.err = model.ErrLandVerificationFailed
	svc := newLandSvc(t, f)
	_, err := svc.Land(context.Background(), "MGIT-1")
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

func TestLandService_ResolveError_Surfaces(t *testing.T) {
	f := defaultFakes(nil, fakePolicy{p: policyOff()})
	f.resolver.err = model.ErrSandboxNotFound
	svc := newLandSvc(t, f)
	_, err := svc.Land(context.Background(), "MGIT-1")
	assert.Error(t, err)
	assert.Zero(t, f.puller.pulls, "no pull when the sandbox does not resolve")
}

func TestNewLandService_NilDeps(t *testing.T) {
	r := &fakeResolver{}
	p := &fakeLandPuller{}
	l := &fakeLedger{}
	pt := &fakeParents{}
	a := &fakeAttestor{}
	o := &fakeOrchestrator{}
	pol := fakePolicy{}
	clk := func() time.Time { return time.Unix(0, 0).UTC() }
	for name, build := range map[string]func() (*LandService, error){
		"nil_resolver": func() (*LandService, error) { return NewLandService(nil, p, l, pt, a, o, pol, clk) },
		"nil_puller":   func() (*LandService, error) { return NewLandService(r, nil, l, pt, a, o, pol, clk) },
		"nil_ledger":   func() (*LandService, error) { return NewLandService(r, p, nil, pt, a, o, pol, clk) },
		"nil_parents":  func() (*LandService, error) { return NewLandService(r, p, l, nil, a, o, pol, clk) },
		"nil_attestor": func() (*LandService, error) { return NewLandService(r, p, l, pt, nil, o, pol, clk) },
		"nil_orch":     func() (*LandService, error) { return NewLandService(r, p, l, pt, a, nil, pol, clk) },
		"nil_policy":   func() (*LandService, error) { return NewLandService(r, p, l, pt, a, o, nil, clk) },
		"nil_clock":    func() (*LandService, error) { return NewLandService(r, p, l, pt, a, o, pol, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := build()
			assert.Error(t, err)
		})
	}
}
