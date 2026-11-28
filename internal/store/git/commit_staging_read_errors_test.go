package git

import (
	"encoding/json"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateCommitPreservesHeadWhenStagingCannotBeRead(t *testing.T) {
	for _, fixture := range []string{"invalid JSON", "directory instead of index"} {
		t.Run(fixture, func(t *testing.T) {
			repo := initTestRepo(t)
			before := writeAndCommit(t, repo, "MGIT-1", map[string]string{"notes": "old"})
			writeFile(t, repo, "notes", "updated")
			require.NoError(t, NewWorktreeStore(repo).Add(t.Context(), "notes"))
			index := repo.stagingPath()
			require.NoError(t, os.Rename(index, index+".saved"))
			if fixture == "invalid JSON" {
				require.NoError(t, os.WriteFile(index, []byte("{"), 0600))
			} else {
				require.NoError(t, os.Mkdir(index, 0700))
			}

			store := NewCommitStore(repo)
			hash, err := store.CreateCommit(t.Context(), makeTestModelCommit(t, "MGIT-1"))
			if fixture == "invalid JSON" {
				var syntax *json.SyntaxError
				require.ErrorAs(t, err, &syntax)
				data, readErr := fs.ReadFile(os.DirFS(repo.MgitDir()), stagingFileName)
				require.NoError(t, readErr)
				require.Equal(t, "{", string(data))
			} else {
				var pathErr *fs.PathError
				require.ErrorAs(t, err, &pathErr)
				require.Equal(t, index, pathErr.Path)
				require.DirExists(t, index)
			}
			require.Empty(t, hash)
			head, err := repo.Head()
			require.NoError(t, err)
			require.Equal(t, before, head)
			require.NoError(t, os.Rename(index, index+".invalid"))
			require.NoError(t, os.Rename(index+".saved", index))
			hash, err = store.CreateCommit(t.Context(), makeTestModelCommit(t, "MGIT-1"))
			require.NoError(t, err)
			data, err := store.GetFileFromCommit(t.Context(), hash, "notes")
			require.NoError(t, err)
			require.Equal(t, "updated", string(data))
		})
	}
}
