package sandboxd

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/landwire"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// --- real land-path fakes (the persister/verifier/policy the orchestrator
// owns; the daemon never references them) -------------------------------

// recordingPersister is the ONLY writer of the shared store on the land path.
// It is reachable in this wiring solely through the orchestrator — the daemon
// holds a SandboxLander, never this. Refs: SEC-01 (no-bypass)
type recordingPersister struct {
	calls   int
	taskID  string
	commits []land.LandedCommit
	pool    []land.Object
}

func (p *recordingPersister) Land(_ context.Context, taskID string, pool []land.Object, commits []land.LandedCommit) error {
	p.calls++
	p.taskID = taskID
	p.pool = pool
	p.commits = commits
	return nil
}

type okVerifier struct{}

func (okVerifier) Verify(context.Context, *model.Attestation) error { return nil }

type okEvents struct{ count int }

func (e *okEvents) AppendSandboxEvent(context.Context, *model.SandboxEvent) error {
	e.count++
	return nil
}

type offPolicy struct{}

func (offPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.SandboxPolicy{RequireSandbox: false}, nil
}

type landResolver struct{ id string }

func (r landResolver) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: r.id}, nil
}

type emptyLedger struct{}

func (emptyLedger) GetTaskCommits(context.Context, string) ([]index.CommitRecord, error) {
	return nil, nil
}

type nopAttestor struct{}

func (nopAttestor) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	return &model.Attestation{SandboxID: sandboxID, CommitHash: commitHash, ContentHash: contentHash}, nil
}

// landAdapter wraps the service land result to the daemon's SandboxLander.
type landAdapter struct{ svc *service.LandService }

func (a landAdapter) Land(ctx context.Context, taskID string) (int, string, error) {
	s, err := a.svc.Land(ctx, taskID)
	if err != nil {
		return 0, "", err
	}
	return s.Commits, s.Branch, nil
}

// --- pool builder --------------------------------------------------------

func framedSingleCommitPool(t *testing.T, task string) []byte {
	t.Helper()
	st := memory.NewStorage()
	put := func(o plumbing.EncodedObject) plumbing.Hash {
		h, err := st.SetEncodedObject(o)
		require.NoError(t, err)
		return h
	}
	rawOf := func(h plumbing.Hash) []byte {
		o, err := st.EncodedObject(plumbing.AnyObject, h)
		require.NoError(t, err)
		r, err := o.Reader()
		require.NoError(t, err)
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		return data
	}
	blobObj := st.NewEncodedObject()
	blobObj.SetType(plumbing.BlobObject)
	bw, _ := blobObj.Writer()
	_, _ = bw.Write([]byte("landed content"))
	_ = bw.Close()
	blob := put(blobObj)

	treeObj := st.NewEncodedObject()
	require.NoError(t, (&object.Tree{Entries: []object.TreeEntry{
		{Name: "a.txt", Mode: filemode.Regular, Hash: blob},
	}}).Encode(treeObj))
	tree := put(treeObj)

	sig := object.Signature{Name: "agent", Email: "a@mgit", When: time.Unix(0, 0).UTC()}
	commitObj := st.NewEncodedObject()
	require.NoError(t, (&object.Commit{Author: sig, Committer: sig, Message: "feat: " + task, TreeHash: tree}).Encode(commitObj))
	commit := put(commitObj)

	var buf bytes.Buffer
	require.NoError(t, landwire.WriteFrame(&buf, landwire.ObjBlob, rawOf(blob)))
	require.NoError(t, landwire.WriteFrame(&buf, landwire.ObjTree, rawOf(tree)))
	require.NoError(t, landwire.WriteFrame(&buf, landwire.ObjCommit, rawOf(commit)))
	return buf.Bytes()
}

// TestDaemon_Land_RoutesThroughVerifiedOrchestrator wires the REAL land path
// — host channel -> LandService -> verified LandOrchestrator -> persister —
// behind the daemon and proves a `mgit sandbox land` request reaches the
// persister ONLY through the orchestrator's host-side verification. The
// daemon's sole land dependency is the SandboxLander ("land this task"); it
// holds no persister/importer/brancher, so no path imports guest objects
// without verification. Refs: MGIT-11.10.10, SEC-01, SEC-06
func TestDaemon_Land_RoutesThroughVerifiedOrchestrator(t *testing.T) {
	skipUnsupportedHostIPC(t)
	const task = "MGIT-11.10.10"
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	// The host land channel: a fake dialer serving a real single-commit pool,
	// authorized against a bound peer. The SAME channel is the orchestrator's
	// stream opener (replay) and the service's puller (single read).
	binder := NewPeerBinder(nil)
	binder.Bind("sbx-1", "sbx-1")
	dialer := &fakeLandDialer{data: framedSingleCommitPool(t, task)}
	channel := NewLandChannel(binder, dialer, land.DefaultLimits(), nil)

	// Host store (for the parent resolver) and the verified orchestrator,
	// constructed with the channel as its stream opener.
	repo, err := git.Init(t.TempDir(), clock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(repo))
	persister := &recordingPersister{}
	events := &okEvents{}
	orch, err := service.NewLandOrchestrator(channel, okVerifier{}, persister, parents, events,
		offPolicy{}, land.DefaultLimits(), clock)
	require.NoError(t, err)

	landSvc, err := service.NewLandService(landResolver{id: "sbx-1"}, channel, emptyLedger{},
		parents, nopAttestor{}, orch, offPolicy{})
	require.NoError(t, err)

	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	cfg.Lander = landAdapter{svc: landSvc}
	cfg.PeerBinder = binder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)

	conn := dialAuthed(t, cfg.SocketPath)
	defer func() { _ = conn.Close() }()
	require.NoError(t, controlproto.WriteRequest(conn, &controlproto.Request{
		Kind: controlproto.KindLand, Land: &controlproto.TaskRef{TaskID: task},
	}))
	resp, err := controlproto.ReadResponse(conn)
	require.NoError(t, err)
	require.Empty(t, resp.Error, "the land succeeds")
	require.NotNil(t, resp.Landed)
	assert.Equal(t, 1, resp.Landed.Commits, "one commit landed")
	assert.Equal(t, "task/"+task, resp.Landed.Branch)

	require.Equal(t, 1, persister.calls, "the persister is reached EXACTLY through the orchestrator")
	require.Len(t, persister.commits, 1)
	assert.Equal(t, task, persister.taskID)
	assert.NotEmpty(t, persister.pool, "the verified object pool is imported")
	assert.Equal(t, 1, events.count, "the land is audited")

	cancel()
	require.NoError(t, <-done)
}
