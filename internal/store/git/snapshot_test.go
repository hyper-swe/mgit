package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func writeFile(t *testing.T, repo *Repository, rel, content string) {
	t.Helper()
	full := filepath.Join(repo.Root(), rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

func TestSnapshotStore_Capture_RecordsTheWorkingTreeWithoutStagingOrCommitting(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()

	writeFile(t, repo, "main.go", "package main\n")
	writeFile(t, repo, "pkg/util.go", "package pkg\n")

	snap, err := ss.Capture(ctx, "MGIT-110", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)
	require.NotNil(t, snap)

	assert.Equal(t, "MGIT-110", snap.TaskID)
	assert.NotEmpty(t, snap.ID)
	assert.NotEmpty(t, snap.CommitHash)
	assert.NotEmpty(t, snap.Fingerprint)
	assert.Equal(t, model.SnapshotTriggerQuiescence, snap.Trigger)
	assert.GreaterOrEqual(t, snap.FileCount, 2)

	// The capture is PASSIVE: it must not stage anything or move any branch.
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Empty(t, staged, "a snapshot must never stage the agent's files")
}

// STRUCTURAL SEPARATION, the property R-H234 made load-bearing. A snapshot
// lives under its own ref namespace as an ORPHAN commit — no parent, reachable
// from no branch — so it cannot be walked into by anything that follows task
// branch ancestry. That is squash and land made impossible by construction
// rather than by a rule someone must remember. Refs: MGIT-110, R-H234
func TestSnapshotStore_Capture_IsAnOrphanUnderItsOwnRefNamespace(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	writeFile(t, repo, "a.txt", "one\n")

	snap, err := ss.Capture(context.Background(), "MGIT-110", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)

	c, err := repo.repo.CommitObject(plumbing.NewHash(snap.CommitHash))
	require.NoError(t, err)
	assert.Equal(t, 0, c.NumParents(), "a snapshot must be an orphan: no ancestry to be walked into")

	// It lives outside refs/heads entirely.
	refs, err := repo.repo.References()
	require.NoError(t, err)
	found := false
	require.NoError(t, refs.ForEach(func(r *plumbing.Reference) error {
		if r.Hash() == plumbing.NewHash(snap.CommitHash) {
			found = true
			assert.True(t, strings.HasPrefix(r.Name().String(), "refs/mgit-snapshots/"),
				"snapshot ref %s must live in the snapshot namespace", r.Name())
			assert.False(t, r.Name().IsBranch(), "a snapshot must never be a branch")
		}
		return nil
	}))
	assert.True(t, found, "the snapshot ref must exist")

	// And the author identity says what it is, for anyone reading raw objects.
	assert.Contains(t, c.Author.Name, "snapshot")
}

func TestSnapshotStore_List_NewestFirst_AndScopedToTheTask(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()

	writeFile(t, repo, "a.txt", "1\n")
	first, err := ss.Capture(ctx, "MGIT-110", base)
	require.NoError(t, err)
	writeFile(t, repo, "a.txt", "2\n")
	second, err := ss.Capture(ctx, "MGIT-110", base.Add(2*time.Minute))
	require.NoError(t, err)
	writeFile(t, repo, "a.txt", "3\n")
	other, err := ss.Capture(ctx, "MGIT-999", base.Add(3*time.Minute))
	require.NoError(t, err)

	got, err := ss.List(ctx, "MGIT-110")
	require.NoError(t, err)
	require.Len(t, got, 2, "only this task's snapshots")
	assert.Equal(t, second.ID, got[0].ID, "newest first")
	assert.Equal(t, first.ID, got[1].ID)

	otherList, err := ss.List(ctx, "MGIT-999")
	require.NoError(t, err)
	require.Len(t, otherList, 1)
	assert.Equal(t, other.ID, otherList[0].ID)
}

func TestSnapshotStore_Prune_KeepsTheNewestAndReportsWhatItDropped(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()

	for i := 0; i < 5; i++ {
		writeFile(t, repo, "a.txt", strings.Repeat("x", i+1)+"\n")
		_, err := ss.Capture(ctx, "MGIT-110", base.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}

	dropped, err := ss.Prune(ctx, "MGIT-110", 2)
	require.NoError(t, err)
	assert.Equal(t, 3, dropped)

	got, err := ss.List(ctx, "MGIT-110")
	require.NoError(t, err)
	require.Len(t, got, 2, "retention keeps the newest N")

	// Pruning to a keep >= count is a no-op, not an error.
	dropped, err = ss.Prune(ctx, "MGIT-110", 10)
	require.NoError(t, err)
	assert.Equal(t, 0, dropped)
}

// The fingerprint is what lets a passive loop skip an unchanged worktree, so
// an idle agent does not accumulate identical snapshots. Refs: MGIT-110
func TestSnapshotStore_Fingerprint_IsStableWhenNothingChanged(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()
	writeFile(t, repo, "a.txt", "same\n")

	one, err := ss.Capture(ctx, "MGIT-110", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)
	two, err := ss.Capture(ctx, "MGIT-110", time.Unix(1700000060, 0).UTC())
	require.NoError(t, err)
	assert.Equal(t, one.Fingerprint, two.Fingerprint)
	assert.Equal(t, one.TreeHash, two.TreeHash, "identical content dedups to the same tree")

	writeFile(t, repo, "a.txt", "different\n")
	three, err := ss.Capture(ctx, "MGIT-110", time.Unix(1700000120, 0).UTC())
	require.NoError(t, err)
	assert.NotEqual(t, one.Fingerprint, three.Fingerprint)
}

// Recovery is the headline property: an interrupted agent that committed
// NOTHING must still be recoverable. Refs: MGIT-110, MGIT-109
func TestSnapshotStore_Materialize_RecoversWorkThatWasNeverCommitted(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()

	writeFile(t, repo, "main.go", "package main // 30 minutes of work\n")
	writeFile(t, repo, "pkg/deep/nested.go", "package deep\n")
	snap, err := ss.Capture(ctx, "MGIT-109", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)

	// The agent is interrupted; nothing was ever committed.
	commits, err := NewCommitStore(repo).ListCommits(ctx)
	require.NoError(t, err)
	for _, c := range commits {
		assert.Contains(t, c.Message, "initial commit",
			"the premise: the agent authored NOTHING, only mgit's own init commit exists")
	}

	dest := filepath.Join(t.TempDir(), "recovered")
	files, err := ss.Materialize(ctx, snap.ID, dest)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, files, 2)

	got, err := os.ReadFile(filepath.Join(dest, "main.go")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "package main // 30 minutes of work\n", string(got))
	nested, err := os.ReadFile(filepath.Join(dest, "pkg", "deep", "nested.go")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "package deep\n", string(nested))
}

// Restoring must never overwrite a live worktree: the work being recovered is
// usually still on disk, and clobbering it would destroy the thing the
// snapshot exists to protect. Refs: MGIT-110
func TestSnapshotStore_Materialize_RefusesANonEmptyDestination(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	ctx := context.Background()
	writeFile(t, repo, "a.txt", "work\n")
	snap, err := ss.Capture(ctx, "MGIT-110", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)

	dest := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dest, "occupied.txt"), []byte("mine\n"), 0o600))

	_, err = ss.Materialize(ctx, snap.ID, dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")

	// The occupant is untouched.
	b, rerr := os.ReadFile(filepath.Join(dest, "occupied.txt")) //nolint:gosec // test temp path
	require.NoError(t, rerr)
	assert.Equal(t, "mine\n", string(b))
}
