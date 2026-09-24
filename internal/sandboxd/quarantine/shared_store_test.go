package quarantine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// THE SHARED STORE MUST NOT SIT INSIDE THE MOUNTED WORKTREE (SEC-03), and the
// answer must not depend on how a path is spelled. Registration and boot ask
// this one function, so they cannot disagree (MGIT-222: a launch with the
// repository root as its worktree was accepted, wrote into the project, and
// was refused only at first boot). A path reached through a symlink, or in
// another letter case on a case-insensitive volume, names the same
// directory: compared by file identity, never by string alone.
// Refs: MGIT-222, SEC-03, R-H300 rule 5
func TestCheckSharedStore_RefusesAWorktreeThatHoldsTheStore(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	store := filepath.Join(repo, ".mgit")
	require.NoError(t, os.MkdirAll(store, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0o750))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(repo, link))
	outside := t.TempDir()

	tests := []struct {
		name     string
		worktree string
		refused  bool
	}{
		{"the_repository_root", repo, true},
		{"a_directory_that_contains_the_repository", filepath.Dir(repo), true},
		{"the_repository_root_through_a_symlink", link, true},
		{"a_directory_outside_the_repository", outside, false},
		{"a_subdirectory_that_does_not_hold_the_store", filepath.Join(repo, "sub"), false},
	}
	if upper := caseVariant(repo); upper != "" {
		tests = append(tests, struct {
			name     string
			worktree string
			refused  bool
		}{"the_repository_root_in_another_case", upper, true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckSharedStore(tt.worktree, store)
			if !tt.refused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, model.ErrSharedStoreReachable), "a refusal is the SEC-03 sentinel: %v", err)
			assert.Contains(t, err.Error(), store, "the refusal names the store")
		})
	}
}

// caseVariant returns repo with its last component upper-cased when that
// spelling reaches the same directory (a case-insensitive volume), else "".
func caseVariant(repo string) string {
	up := filepath.Join(filepath.Dir(repo), strings.ToUpper(filepath.Base(repo)))
	a, errA := os.Stat(repo)
	b, errB := os.Stat(up)
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		return ""
	}
	return up
}
