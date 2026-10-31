package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

func stepDiffs(e *testEnv) *DiffService {
	return NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx)
}

// The first step is listed with its files even though its parent is the fork
// base and not a task commit, which DiffCommits refuses. Refs: MGIT-297
func TestDiffTaskSteps_FirstStepOnTheForkBaseIsListedWithItsFiles(t *testing.T) {
	e := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommitFiles(t, e, "MGIT-1", "base", map[string]string{"base.txt": "base\n"})
	first := stageAndCommitFiles(t, e, "MGIT-9", "one", map[string]string{"a.txt": "one\n"})
	stageAndCommitFiles(t, e, "MGIT-9", "two", map[string]string{"b.txt": "two\n"})

	steps, err := stepDiffs(e).DiffTaskSteps(ctx, "MGIT-9")
	require.NoError(t, err)
	require.Len(t, steps, 2)
	require.Equal(t, first.CommitID, steps[0].Record.CommitHash)
	require.Equal(t, []string{"a.txt"}, pathsOf(steps[0].Files))
	require.Equal(t, []string{"b.txt"}, pathsOf(steps[1].Files), "a later step is measured against the step before it")
}

// A retained but undelivered commit is neither listed nor used as a
// comparison point. Refs: MGIT-297
func TestDiffTaskSteps_AnUndeliveredCommitIsNotAStepNorABase(t *testing.T) {
	e, pending := pendingContentFixture(t)
	steps, err := stepDiffs(e).DiffTaskSteps(context.Background(), "MGIT-323")
	require.NoError(t, err)
	require.NotEmpty(t, steps)
	var paths []string
	for _, st := range steps {
		require.NotEqual(t, pending, st.Record.CommitHash, "the undelivered commit is not a step")
		paths = append(paths, pathsOf(st.Files)...)
	}
	require.Contains(t, paths, "accepted.txt")
}

// A first commit whose parent is not the task's pinned fork base is refused,
// not trusted as the comparison point. Refs: MGIT-297
func TestDiffTaskSteps_AParentThatIsNotThePinnedForkBaseIsRefused(t *testing.T) {
	e := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommitFiles(t, e, "MGIT-1", "base", map[string]string{"base.txt": "base\n"})
	stageAndCommitFiles(t, e, "MGIT-2", "other", map[string]string{"o.txt": "o\n"})
	stageAndCommitFiles(t, e, "MGIT-9", "one", map[string]string{"a.txt": "one\n"})
	require.NoError(t, e.idx.InsertWorktree(ctx, &model.WorktreeInfo{
		Path: filepath.Join(t.TempDir(), "wt"), Name: "wt", Branch: "wt", TaskID: "MGIT-9", ForkBase: "0000000000000000000000000000000000000001",
	}))
	_, err := stepDiffs(e).DiffTaskSteps(ctx, "MGIT-9")
	require.ErrorIs(t, err, model.ErrVerificationFailed)
}

// An ambiguous ledger refuses the listing, as it refuses every other reader.
// Refs: MGIT-297
func TestDiffTaskSteps_AmbiguousLegacyIsRefused(t *testing.T) {
	e := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommitFiles(t, e, "MGIT-1", "base", map[string]string{"base.txt": "base\n"})
	c := &model.Commit{TaskID: contentTestTaskID(t), AgentID: "local", Message: "[MGIT:MGIT-323] legacy"}
	require.NoError(t, os.WriteFile(filepath.Join(e.repo.Root(), "old.txt"), []byte("old\n"), 0o600))
	require.NoError(t, e.wt.Add(ctx, "old.txt"))
	hash, err := e.cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	require.NoError(t, e.idx.AppendTaskCommit(ctx, index.TaskCommitInsert{TaskID: "MGIT-323", CommitHash: hash, ContentHash: c.ContentHash}))
	_, err = stepDiffs(e).DiffTaskSteps(ctx, "MGIT-323")
	require.ErrorContains(t, err, "ambiguous content eligibility")
}

func pathsOf(files []model.FileDiff) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}
