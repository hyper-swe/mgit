package land

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

const hostReceiveTime = "2026-06-16T18:00:00Z"

// fakeImporter records imported objects and can fail on a chosen commit.
type fakeImporter struct {
	imported  int
	failOnNth int // 1-based; 0 = never fail
	calls     int
}

func (f *fakeImporter) ImportObjects(_ context.Context, objs []Object) error {
	f.calls++
	if f.failOnNth != 0 && f.calls == f.failOnNth {
		return errors.New("object import failed")
	}
	f.imported += len(objs)
	return nil
}

// fakeBrancher records fast-forward calls.
type fakeBrancher struct {
	ffTask, ffCommit string
	calls            int
	err              error
}

func (f *fakeBrancher) FastForward(_ context.Context, taskID, commitHash string) error {
	f.calls++
	f.ffTask, f.ffCommit = taskID, commitHash
	return f.err
}

// newIndex opens a real index store over a temp DB with a fixed host clock.
func newIndex(t *testing.T) *index.Store {
	t.Helper()
	host, err := time.Parse(time.RFC3339, hostReceiveTime)
	require.NoError(t, err)
	s, err := index.New(filepath.Join(t.TempDir(), "idx.db"), func() time.Time { return host })
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// landed builds a LandedCommit with a self-consistent commit.
func landed(commitID, contentHash string, pos int, sandboxID *string) LandedCommit {
	return LandedCommit{
		Commit:    &model.Commit{CommitID: commitID, ContentHash: contentHash, AgentID: "agent-1"},
		SandboxID: sandboxID,
		Position:  pos,
	}
}

// poolFor builds the land's object pool: one commit object per commit.
func poolFor(commits ...LandedCommit) []Object {
	objs := make([]Object, len(commits))
	for i, c := range commits {
		objs[i] = Object{Type: ObjCommit, Data: []byte(c.Commit.CommitID)}
	}
	return objs
}

func sptr(s string) *string { return &s }

// TestLand_Success_FastForwardAppendOnly verifies a successful land
// appends every commit and fast-forwards the task branch to the last
// commit. Refs: FR-17.5
func TestLand_Success_FastForwardAppendOnly(t *testing.T) {
	idx := newIndex(t)
	imp := &fakeImporter{}
	br := &fakeBrancher{}
	l := NewLander(imp, idx, br)

	commits := []LandedCommit{
		landed("1111111111111111111111111111111111111111", "a1", 0, sptr("01JXSBSANDBOX0000000000000")),
		landed("2222222222222222222222222222222222222222", "a2", 1, sptr("01JXSBSANDBOX0000000000000")),
	}
	require.NoError(t, l.Land(context.Background(), "MGIT-11.8.5", poolFor(commits...), commits))

	rows, err := idx.GetTaskCommits(context.Background(), "MGIT-11.8.5")
	require.NoError(t, err)
	require.Len(t, rows, 2, "both commits landed")
	assert.Equal(t, 1, br.calls, "branch fast-forwarded once")
	assert.Equal(t, "MGIT-11.8.5", br.ffTask)
	assert.Equal(t, "2222222222222222222222222222222222222222", br.ffCommit, "FF to the last commit")
	assert.Equal(t, 2, imp.imported, "all objects imported")
	assert.Equal(t, 1, imp.calls, "the pool is imported in one call")
}

// TestLand_PartialFailure_RollsBackAll verifies that a failure during
// the land leaves NO partial state: neither an object-import failure nor
// a mid-batch append failure lands any commit. Refs: FR-17.5
func TestLand_PartialFailure_RollsBackAll(t *testing.T) {
	t.Run("object_import_fails", func(t *testing.T) {
		idx := newIndex(t)
		br := &fakeBrancher{}
		l := NewLander(&fakeImporter{failOnNth: 1}, idx, br)
		commits := []LandedCommit{
			landed("1111111111111111111111111111111111111111", "a1", 0, nil),
			landed("2222222222222222222222222222222222222222", "a2", 1, nil),
		}
		require.Error(t, l.Land(context.Background(), "T", poolFor(commits...), commits))
		rows, _ := idx.GetTaskCommits(context.Background(), "T")
		assert.Empty(t, rows, "no commit lands if the pool import fails")
		assert.Zero(t, br.calls, "branch is not advanced on failure")
	})

	t.Run("append_batch_rolls_back_on_duplicate", func(t *testing.T) {
		idx := newIndex(t)
		br := &fakeBrancher{}
		l := NewLander(&fakeImporter{}, idx, br)
		// Two commits with the SAME id violate UNIQUE(task_id, commit_hash)
		// on the second insert — the whole batch tx must roll back.
		dup := "3333333333333333333333333333333333333333"
		commits := []LandedCommit{landed(dup, "a1", 0, nil), landed(dup, "a2", 1, nil)}
		require.Error(t, l.Land(context.Background(), "T", poolFor(commits...), commits))
		rows, _ := idx.GetTaskCommits(context.Background(), "T")
		assert.Empty(t, rows, "a mid-batch insert failure rolls back the whole batch")
		assert.Zero(t, br.calls)
	})
}

// TestLand_HostReceiveTimeRecorded verifies the landed row carries the
// host receive-time (not the guest's advisory timestamp). Refs: SEC-11, FR-17.28
func TestLand_HostReceiveTimeRecorded(t *testing.T) {
	idx := newIndex(t)
	l := NewLander(&fakeImporter{}, idx, &fakeBrancher{})

	c := landed("4444444444444444444444444444444444444444", "a1", 0, nil)
	// The guest claims a wildly different (advisory) timestamp.
	c.Commit.CreatedAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, l.Land(context.Background(), "T", poolFor(c), []LandedCommit{c}))

	rows, err := idx.GetTaskCommits(context.Background(), "T")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, hostReceiveTime, rows[0].CreatedAt, "task_commits records the host receive-time")
}

// TestLand_SandboxProvenanceRecorded verifies the gate's sandbox_id (nil
// = NULL) is persisted as provenance. Refs: FR-17.6, F-02
func TestLand_SandboxProvenanceRecorded(t *testing.T) {
	idx := newIndex(t)
	l := NewLander(&fakeImporter{}, idx, &fakeBrancher{})
	commits := []LandedCommit{
		landed("5555555555555555555555555555555555555555", "a1", 0, sptr("01JXSBSANDBOX0000000000000")),
		landed("6666666666666666666666666666666666666666", "a2", 1, nil), // unsandboxed → NULL
	}
	require.NoError(t, l.Land(context.Background(), "T", poolFor(commits...), commits))
	rows, err := idx.GetTaskCommits(context.Background(), "T")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].SandboxID)
	assert.Equal(t, "01JXSBSANDBOX0000000000000", *rows[0].SandboxID)
	assert.Nil(t, rows[1].SandboxID, "unsandboxed commit records NULL provenance")
}

// TestLand_Empty_NoOp verifies landing nothing is a no-op (no FF).
func TestLand_Empty_NoOp(t *testing.T) {
	br := &fakeBrancher{}
	l := NewLander(&fakeImporter{}, newIndex(t), br)
	require.NoError(t, l.Land(context.Background(), "T", nil, nil))
	assert.Zero(t, br.calls)
}

// TestLand_BrancherError_Surfaces verifies a fast-forward failure is
// reported (land is not silently successful once rows are appended).
func TestLand_BrancherError_Surfaces(t *testing.T) {
	idx := newIndex(t)
	l := NewLander(&fakeImporter{}, idx, &fakeBrancher{err: errors.New("non-fast-forward")})
	c := landed("7777777777777777777777777777777777777777", "a1", 0, nil)
	err := l.Land(context.Background(), "T", poolFor(c), []LandedCommit{c})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fast-forward")
}
