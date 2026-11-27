package git

import (
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

func TestDiffCommitPreservesTipAndStagingOnStoreFailure(t *testing.T) {
	for _, stage := range []string{"resolve HEAD", "get HEAD commit", "object store write failure", "update ref"} {
		t.Run(stage, func(t *testing.T) {
			repo, faults, tip := faultRepoWithCommit(t)
			stageOneFile(t, repo, "pending", "separate change")
			hash, err := writeBlob(repo.repo.Storer, []byte("selected change"))
			require.NoError(t, err)
			diffs := []model.FileDiff{{Path: "selected", Operation: model.DiffAdded, NewHash: hash.String()}}
			cs := NewCommitStore(repo)
			create := func() (string, error) {
				return cs.CreateCommitFromDiffs(t.Context(), makeTestModelCommit(t, "MGIT-9.2"), diffs)
			}
			switch stage {
			case "resolve HEAD":
				faults.failGetReference = true
			case "get HEAD commit":
				faults.failGetObject = true
			case "object store write failure":
				faults.failSetObject = true
			case "update ref":
				faults.failCAS = true
			}
			failed, err := create()
			faults.failGetReference, faults.failGetObject, faults.failSetObject, faults.failCAS = false, false, false, false
			require.ErrorContains(t, err, stage)
			require.Empty(t, failed)
			current, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, tip, current)
			staged, err := repo.StagedSnapshot()
			require.NoError(t, err)
			require.Equal(t, []string{"pending"}, staged)

			next, err := create()
			require.NoError(t, err)
			current, err = repo.Head()
			require.NoError(t, err)
			require.Equal(t, next, current)
			data, err := cs.GetFileFromCommit(t.Context(), next, "selected")
			require.NoError(t, err)
			require.Equal(t, "selected change", string(data))
			_, err = cs.GetFileFromCommit(t.Context(), next, "pending")
			require.Error(t, err)
			staged, err = repo.StagedSnapshot()
			require.NoError(t, err)
			require.Equal(t, []string{"pending"}, staged)
		})
	}
}
