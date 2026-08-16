package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PathsMatchingCommitted decides what a read verb's auto-resync is allowed to
// absorb into the base: only content the user's git has actually committed.
// Everything it excludes stays uncommitted, visible to `mgit status` and
// attributable to a task — the MGIT-123 guarantee. Refs: MGIT-123, ADR-008 §3

// blobID returns the git blob id content hashes to.
func blobID(content string) string {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content)).String()
}

// TestPathsMatchingCommitted_ClassifiesWorkingTreeAgainstGit covers each way a
// working file can relate to git's committed tree. Refs: MGIT-123
func TestPathsMatchingCommitted_ClassifiesWorkingTreeAgainstGit(t *testing.T) {
	tests := []struct {
		name      string
		onDisk    map[string]string
		committed map[string]string
		paths     []string
		want      []string
	}{
		{
			name:      "unchanged_tracked_file_is_committed_content",
			onDisk:    map[string]string{"a.go": "v1\n"},
			committed: map[string]string{"a.go": blobID("v1\n")},
			paths:     []string{"a.go"},
			want:      []string{"a.go"},
		},
		{
			name:      "modified_tracked_file_is_uncommitted_work",
			onDisk:    map[string]string{"a.go": "v2\n"},
			committed: map[string]string{"a.go": blobID("v1\n")},
			paths:     []string{"a.go"},
			want:      nil,
		},
		{
			name:      "untracked_file_is_uncommitted_work",
			onDisk:    map[string]string{"new.go": "new\n"},
			committed: map[string]string{},
			paths:     []string{"new.go"},
			want:      nil,
		},
		{
			name:      "empty_committed_set_absorbs_nothing",
			onDisk:    map[string]string{"a.go": "v1\n"},
			committed: nil,
			paths:     []string{"a.go"},
			want:      nil,
		},
		{
			name:   "mixed_set_keeps_only_the_committed_ones",
			onDisk: map[string]string{"keep.go": "k\n", "edited.go": "v2\n", "new.go": "n\n"},
			committed: map[string]string{
				"keep.go":   blobID("k\n"),
				"edited.go": blobID("v1\n"),
			},
			paths: []string{"edited.go", "keep.go", "new.go"},
			want:  []string{"keep.go"},
		},
		{
			name:      "case_differing_path_is_not_a_match",
			onDisk:    map[string]string{"src/Auth.tsx": "x\n"},
			committed: map[string]string{"src/auth.tsx": blobID("x\n")},
			paths:     []string{"src/Auth.tsx"},
			want:      nil,
		},
		{
			name:      "no_paths_yields_nothing",
			onDisk:    map[string]string{},
			committed: map[string]string{"a.go": blobID("v1\n")},
			paths:     nil,
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			for rel, content := range tt.onDisk {
				abs := filepath.Join(repo.Root(), filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
				require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
			}

			got, err := repo.PathsMatchingCommitted(tt.committed, tt.paths)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestPathsMatchingCommitted_MissingWorkingFile_ReturnsError verifies the read
// fails loud rather than silently treating an unreadable path as a match — a
// silent match would absorb content into the base. Refs: MGIT-123
func TestPathsMatchingCommitted_MissingWorkingFile_ReturnsError(t *testing.T) {
	repo := initTestRepo(t)

	_, err := repo.PathsMatchingCommitted(
		map[string]string{"gone.go": blobID("x\n")}, []string{"gone.go"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone.go")
}
