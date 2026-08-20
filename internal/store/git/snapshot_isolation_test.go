package git

import (
	"context"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// THE PROMISES, AS TESTS.
//
// R-H234 made the separation load-bearing: a snapshot rendered next to
// authored commits "reintroduces the confusion the split exists to remove",
// and snapshots "must never be squashed or landed: they are evidence, not
// content". Those are claims about behavior, and a claim about behavior that
// is not executed is a decorative declaration — the class this repo keeps
// finding. So each one is asserted here against the REAL reader that would
// have to obey it, not against a rule in a comment.
//
// Refs: MGIT-110, R-H234

// snapshotAndCommit sets up a task with one authored commit and one passive
// snapshot taken afterwards, which is the arrangement where a leak would show.
func snapshotAndCommit(t *testing.T) (*Repository, *model.Snapshot) {
	t.Helper()
	repo := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, repo, "authored.txt", "the agent's work\n")
	require.NoError(t, repo.stagePaths([]string{"authored.txt"}))
	_, err := NewCommitStore(repo).CreateCommit(ctx, makeTestModelCommit(t, "MGIT-110"))
	require.NoError(t, err)

	writeFile(t, repo, "uncommitted.txt", "work the agent never committed\n")
	snap, err := NewSnapshotStore(repo).Capture(ctx, "MGIT-110", time.Unix(1700000000, 0).UTC())
	require.NoError(t, err)
	return repo, snap
}

// A snapshot must never appear in the commit log a reviewer reads.
func TestSnapshot_NeverAppearsInTheCommitLog(t *testing.T) {
	repo, snap := snapshotAndCommit(t)

	commits, err := NewCommitStore(repo).ListCommits(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, commits, "the authored commit must be there")
	for _, c := range commits {
		assert.NotEqual(t, snap.CommitHash, c.CommitID,
			"a passive snapshot surfaced in the authored trail")
		assert.NotContains(t, c.Message, "mgit snapshot",
			"a snapshot's message reached the log a reviewer reads")
	}
}

// A snapshot must never be reachable from the task branch — which is what
// makes squash and land structurally unable to include it.
func TestSnapshot_IsUnreachableFromTheTaskBranch(t *testing.T) {
	repo, snap := snapshotAndCommit(t)

	head, err := repo.currentRef()
	require.NoError(t, err)
	iter, err := repo.repo.Log(&gogit.LogOptions{From: head.Hash()})
	require.NoError(t, err)
	defer iter.Close()

	snapHash := plumbing.NewHash(snap.CommitHash)
	require.NoError(t, iter.ForEach(func(c *object.Commit) error {
		assert.NotEqual(t, snapHash, c.Hash, "the snapshot is reachable by walking the branch")
		return nil
	}))
}

// The squash reads a task's authored commits. A snapshot must not be among
// them, so the artifact that lands in the user's repository cannot carry one.
func TestSnapshot_IsNotSquashedIntoTheLandedArtifact(t *testing.T) {
	repo, snap := snapshotAndCommit(t)
	ctx := context.Background()

	commits, err := NewCommitStore(repo).ListCommits(ctx)
	require.NoError(t, err)
	hashes := make([]string, 0, len(commits))
	for _, c := range commits {
		hashes = append(hashes, c.CommitID)
		assert.NotEqual(t, snap.CommitHash, c.CommitID)
	}

	preview, err := NewCommitStore(repo).BuildSquashTree(ctx, hashes)
	require.NoError(t, err)

	// The file that exists ONLY in the snapshot must not appear in the tree the
	// squash would land. Reading the real tree, not a summary of it.
	entries, err := NewTreeStore(repo).TraverseTree(ctx, preview.Tree)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "uncommitted.txt", e.Path,
			"a snapshot's uncommitted file reached the squashed artifact")
	}
	assert.NotEmpty(t, entries, "the squash tree must still carry the authored work")
}

// Retention belongs to the snapshot namespace alone: pruning snapshots must
// never disturb the authored trail.
func TestSnapshot_PruneLeavesAuthoredCommitsUntouched(t *testing.T) {
	repo, _ := snapshotAndCommit(t)
	ctx := context.Background()
	ss := NewSnapshotStore(repo)

	before, err := NewCommitStore(repo).ListCommits(ctx)
	require.NoError(t, err)

	writeFile(t, repo, "more.txt", "later\n")
	_, err = ss.Capture(ctx, "MGIT-110", time.Unix(1700000600, 0).UTC())
	require.NoError(t, err)
	dropped, err := ss.Prune(ctx, "MGIT-110", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, dropped)

	after, err := NewCommitStore(repo).ListCommits(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(before), len(after), "pruning evidence must not touch the narrative")
}
