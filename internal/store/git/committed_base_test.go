package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Every state git has not committed is listed: an edit, an untracked file and
// a committed file missing from disk; a file matching git is not. Refs: MGIT-283
func TestUncommittedAgainst_ListsEditsUntrackedAndMissing(t *testing.T) {
	repo := initTestRepo(t)
	writeFiles(t, repo, map[string]string{"same.go": "same\n", "edited.go": "new\n", "untracked.go": "u\n"})
	committed := map[string]string{
		"same.go": blobID("same\n"), "edited.go": blobID("old\n"), "gone.go": blobID("g\n"),
	}

	got, err := repo.UncommittedAgainst(committed)
	require.NoError(t, err)
	assert.Equal(t, []string{"edited.go", "gone.go", "untracked.go"}, got)
}

// The detached commit's tree is exactly the snapshot, and no branch moves.
// Refs: MGIT-283
func TestCreateDetachedCommit_TreeIsTheSnapshotAndNoRefMoves(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	writeAndCommit(t, repo, "MGIT-1", map[string]string{"local-only.go": "x\n"})
	before, err := repo.Head()
	require.NoError(t, err)

	id, err := cs.CreateDetachedCommit(&model.Commit{AgentID: "mgit-sync", Message: "fork base"}, before,
		[]SnapshotFile{{Path: "pkg/a.go", Mode: filemode.Regular, Content: []byte("a\n")}})
	require.NoError(t, err)

	after, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, before, after)
	content, err := cs.GetFileFromCommit(context.Background(), id, "pkg/a.go")
	require.NoError(t, err)
	assert.Equal(t, "a\n", string(content))
	_, err = cs.GetFileFromCommit(context.Background(), id, "local-only.go")
	assert.Error(t, err, "the snapshot is the whole tree; nothing is layered on the parent's")
}

func TestCreateDetachedCommit_EscapingPath_Refused(t *testing.T) {
	repo := initTestRepo(t)
	_, err := NewCommitStore(repo).CreateDetachedCommit(&model.Commit{AgentID: "mgit-sync"}, "",
		[]SnapshotFile{{Path: "../outside", Mode: filemode.Regular, Content: []byte("x")}})
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(filepath.Dir(repo.Root()), "outside"))
	assert.True(t, os.IsNotExist(statErr))
}

// When the snapshot's tree is the parent's own tree there is nothing to
// record: the parent is returned and no commit is written. Refs: MGIT-283
func TestCreateDetachedCommit_SameTreeAsParent_ReturnsTheParent(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	parent := writeAndCommit(t, repo, "MGIT-1", map[string]string{"a.go": "a\n"})
	before, err := cs.ListCommits(context.Background())
	require.NoError(t, err)

	id, err := cs.CreateDetachedCommit(&model.Commit{AgentID: "mgit-sync", Message: "fork base"}, parent,
		[]SnapshotFile{{Path: "a.go", Mode: filemode.Regular, Content: []byte("a\n")}})
	require.NoError(t, err)
	assert.Equal(t, parent, id)
	after, err := cs.ListCommits(context.Background())
	require.NoError(t, err)
	assert.Len(t, after, len(before), "no commit is written")
}
