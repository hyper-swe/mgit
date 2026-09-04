package microvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// fakeSettler stands in for the guest's own view of the delivered tree. It
// answers "stale" for the first `staleFor` probes and "settled" after, or
// never settles at all — the two shapes the daemon must tell apart.
type fakeSettler struct {
	mu       sync.Mutex
	staleFor int
	never    bool
	calls    []settleRequest
	probes   int
}

func (f *fakeSettler) Probe(_ context.Context, req settleRequest) (settleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	if len(f.calls) == 0 || f.calls[len(f.calls)-1].sandboxID != req.sandboxID {
		f.calls = append(f.calls, req)
	}
	stale := f.never || f.probes <= f.staleFor
	view := settleView{}
	if stale {
		for path := range req.want {
			view.stale = append(view.stale, path+" (guest reads the old bytes)")
		}
	}
	return view, nil
}

// A sync that applied changes must not report them delivered until the GUEST
// reads what was staged. The host-side read-back (MGIT-164) hashes the host's
// directory, where the bytes are complete; the guest's kernel keeps its own
// view for a window after its last access, and a host write inside that
// window is invisible to it — measured on libkrun as stale stat and stale
// reads for 0.1s to over 1.2s after sync had returned. Refs: MGIT-192
func TestManager_SyncWorktree_ReportsDeliveredOnlyAfterTheGuestSeesIt(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	settler := &fakeSettler{staleFor: 2}
	f.mgr.settler = settler
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	report, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.NoError(t, err)
	assert.Equal(t, []string{"app.go"}, report.Updated)
	require.Len(t, settler.calls, 1, "the guest is asked exactly once per sync")
	want := settler.calls[0].want
	require.Contains(t, want, "app.go")
	assert.Equal(t, worktreesync.Manifest{"app.go": want["app.go"]}["app.go"].Hash, want["app.go"].Hash)
	assert.NotEmpty(t, want["app.go"].Hash, "the guest is asked to confirm the STAGED digest, not just presence")
	assert.Equal(t, 3, settler.probes, "two stale probes, then the settled one")
}

// A guest that never settles is a refusal naming the paths and the bound —
// never a success on the host's view alone. Refs: MGIT-192
func TestManager_SyncWorktree_GuestThatNeverSettles_IsRefusedNamingThePaths(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	f.mgr.settler = &fakeSettler{never: true}
	f.mgr.settleBudget = 30 * 1e6 // 30ms: the bound is a parameter, and the test must not wait the real one
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	report, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGuestViewNotSettled), "a classifiable refusal: %v", err)
	assert.Contains(t, err.Error(), "app.go")
	assert.Contains(t, err.Error(), "did not settle")
	if report != nil {
		assert.Empty(t, report.Updated, "nothing is reported delivered")
	}
}

// An unchanged host costs one staging build and no guest round trip — the
// affordability the automatic pre-exec sync rests on. Refs: MGIT-192, MGIT-71
func TestManager_SyncWorktree_NoChanges_DoesNotAskTheGuest(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	settler := &fakeSettler{}
	f.mgr.settler = settler

	report, err := f.mgr.SyncWorktree(context.Background(), f.id, model.WorktreeSyncOptions{})

	require.NoError(t, err)
	assert.True(t, report.Skipped)
	assert.Equal(t, 0, settler.probes)
}

// The automatic sync before an exec takes the same door: a guest that has not
// settled refuses the exec rather than running a command against a tree the
// guest cannot yet read. Refs: MGIT-192, MGIT-71
func TestManager_SyncBeforeExec_RefusesWhenTheGuestDoesNotSettle(t *testing.T) {
	f := newSyncFixture(t, map[string]string{"app.go": "V1"})
	f.mgr.settler = &fakeSettler{never: true}
	f.mgr.settleBudget = 30 * 1e6
	writeFiles(t, f.worktree, map[string]string{"app.go": "V2"})

	sb := f.mgr.sandboxes[f.id]
	err := f.mgr.syncBeforeExec(context.Background(), sb)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGuestViewNotSettled))
	assert.Contains(t, err.Error(), "exec refused")
}

// The exec-based settler's pure parts: what it asks the guest to run, and how
// it reads the answer. The rows come from sha256sum's documented output and
// from the shapes a guest can answer with, not from the implementation.
func TestExecSettler_ReadsSha256sumOutput(t *testing.T) {
	out := "abc123  app.go\n" +
		"def456  dir/with space.txt\n" +
		"sha256sum: gone.go: No such file or directory\n"
	got := parseSha256sum(out)
	assert.Equal(t, "abc123", got["app.go"])
	assert.Equal(t, "def456", got["dir/with space.txt"])
	_, present := got["gone.go"]
	assert.False(t, present, "a missing file has no digest")
}

func TestExecSettler_ClassifiesTheGuestView(t *testing.T) {
	want := worktreesync.Manifest{
		"same.go":  {Hash: "aaa", Mode: 0o644},
		"stale.go": {Hash: "bbb", Mode: 0o644},
		"gone.go":  {Hash: "ccc", Mode: 0o644},
		"link":     {Hash: "ddd", Mode: os.ModeSymlink | 0o777},
	}
	got := map[string]string{"same.go": "aaa", "stale.go": "OLD"}
	view := classifyGuestView(want, got, []string{"deleted.go"}, []string{"deleted.go"})
	assert.ElementsMatch(t, []string{
		"stale.go (guest reads bytes that were not delivered)",
		"gone.go (guest cannot read it)",
		"deleted.go (still present in the guest)",
	}, view.stale)
	assert.NotContains(t, strings.Join(view.stale, ","), "link", "symlinks are recorded by target text and are not hashed in the guest")
}

func TestExecSettler_ChunksLongArgv(t *testing.T) {
	paths := make([]string, 0, 9000)
	for i := 0; i < 9000; i++ {
		paths = append(paths, filepath.Join("d", "f"+strings.Repeat("x", i%7)+".go"))
	}
	chunks := chunkPaths(paths, 4000)
	require.Len(t, chunks, 3)
	assert.Len(t, chunks[0], 4000)
	assert.Len(t, chunks[2], 1000)
}
