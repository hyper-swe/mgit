package git

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// Refs: FR-2.6, MGIT-288. Plain paths keep the existing command spelling;
// shell metacharacters and option-like paths must survive copy/paste.
func TestStagedEntry_Remedy_QuotesOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct{ path, arg string }{
		{"pkg/tr", "pkg/tr"},
		{"pkg/@x+y=1:a,b%z", "pkg/@x+y=1:a,b%z"},
		{"a/b c", "'a/b c'"},
		{"a/b'c", "'a/b'\\''c'"},
		{"a/$HOME", "'a/$HOME'"},
		{"a/*.go", "'a/*.go'"},
		{"-dir", "./-dir"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			repo := initTestRepo(t)
			writeFiles(t, repo, map[string]string{tc.path + "/file": "content"})
			err := repo.checkStagedEntry(tc.path, nil)
			require.ErrorIs(t, err, model.ErrInvalidStagedEntry)
			assert.Contains(t, err.Error(), "`mgit restore --staged "+tc.arg+"`")
			assert.Contains(t, err.Error(), "`mgit add "+tc.arg+"`")
			assert.Contains(t, err.Error(), "also unstages the files under it")
		})
	}
}

// Refusal lists every invalid directory before any staged content is applied.
// Refs: FR-2.6, MGIT-288.
func TestCommit_MultipleStagedDirectories_NamesAllWithoutChangingState(t *testing.T) {
	repo := initTestRepo(t)
	paths := []string{"a/b c", "z/other'entry", "keep.txt"}
	writeFiles(t, repo, map[string]string{paths[0] + "/file": "a", paths[1] + "/file": "b", paths[2]: "keep"})
	require.NoError(t, repo.saveStaging(&stagingState{Paths: append([]string(nil), paths...)}))
	before, err := repo.repo.Head()
	require.NoError(t, err)
	_, err = NewCommitStore(repo).CreateCommit(context.Background(), makeTestModelCommit(t, "MGIT-288"))
	require.ErrorIs(t, err, model.ErrInvalidStagedEntry)
	for _, p := range paths[:2] {
		assert.Contains(t, err.Error(), fmt.Sprintf("%q is a directory", p))
	}
	assert.ElementsMatch(t, paths, stagedNow(t, repo))
	after, err := repo.repo.Head()
	require.NoError(t, err)
	assert.Equal(t, before.Hash(), after.Hash())
}
