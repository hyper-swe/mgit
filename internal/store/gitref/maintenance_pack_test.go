package gitref

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stock Git incremental maintenance uses loose-*.pack, not pack-*.pack.
// Reading either a normal repository or a linked worktree must preserve Git.
// Refs: FEAT-3.153
func TestCommittedContent_LooseMaintenancePack(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "normal"
		if linked {
			name = "linked"
		}
		t.Run(name, func(t *testing.T) {
			root := gitRepoWithCommit(t, "file.txt", "committed\n")
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", root}, args...)...) //nolint:gosec // fixed Git verbs, test-owned root
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				return string(out)
			}
			project := root
			if linked {
				project = filepath.Join(t.TempDir(), "linked")
				run("worktree", "add", "--detach", project, "HEAD")
			}
			run("maintenance", "run", "--task=loose-objects")
			run("maintenance", "run", "--task=loose-objects")
			run("fsck", "--full")
			packs, err := filepath.Glob(filepath.Join(root, ".git", "objects", "pack", "loose-*.pack"))
			require.NoError(t, err)
			require.NotEmpty(t, packs, "fixture must really contain Git maintenance packs")
			before := dotGitSnapshot(t, root)
			blobs, err := CommittedBlobs(project)
			require.NoError(t, err)
			assert.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("committed\n")).String(), blobs["file.txt"])
			files, head, err := CommittedFiles(project)
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Len(t, head, 40)
			assert.Equal(t, "committed\n", string(files[0].Content))
			assert.Equal(t, before, dotGitSnapshot(t, root), "reads must not rename or rewrite Git packs")
		})
	}
}

// Git reads indexed packs regardless of basename. Also cover duplicate packs
// and an orphan pack without an index; aliases must not hide valid objects.
func TestCommittedContent_ArbitraryIndexedPackNames(t *testing.T) {
	for _, stem := range []string{"archive", "pack-not-a-checksum", "loose-custom", "pack-0000000000000000000000000000000000000000", "canonical"} {
		t.Run(stem, func(t *testing.T) {
			root := gitRepoWithCommit(t, "file.txt", "committed\n")
			run := func(args ...string) {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput() //nolint:gosec // fixed Git verbs, test-owned root
				require.NoError(t, err, "%s", out)
			}
			run("maintenance", "run", "--task=loose-objects")
			run("maintenance", "run", "--task=loose-objects")
			dir := filepath.Join(root, ".git", "objects", "pack")
			packs, err := filepath.Glob(filepath.Join(dir, "loose-*.pack"))
			require.NoError(t, err)
			require.Len(t, packs, 1)
			original := strings.TrimSuffix(packs[0], ".pack")
			if stem == "canonical" {
				stem = "pack-" + strings.TrimPrefix(filepath.Base(original), "loose-")
			}
			for _, ext := range []string{".pack", ".idx", ".rev"} {
				err = os.Rename(original+ext, filepath.Join(dir, stem+ext))
				if ext == ".rev" && os.IsNotExist(err) {
					continue
				}
				require.NoError(t, err)
			}
			// Duplicate under a canonical-looking name; the actual index checksum,
			// not either basename, is the identifier go-git must see.
			for _, ext := range []string{".pack", ".idx"} {
				bytes, readErr := os.ReadFile(filepath.Join(dir, stem+ext)) //nolint:gosec // fixed fixture names in test-owned root
				require.NoError(t, readErr)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "duplicate"+ext), bytes, 0600)) //nolint:gosec // fixed fixture names in test-owned root
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "unindexed.pack"), []byte("not indexed"), 0600))
			run("cat-file", "-p", "HEAD:file.txt")
			before := dotGitSnapshot(t, root)
			blobs, err := CommittedBlobs(root)
			require.NoError(t, err)
			require.Contains(t, blobs, "file.txt")
			files, _, err := CommittedFiles(root)
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, "committed\n", string(files[0].Content))
			assert.Equal(t, before, dotGitSnapshot(t, root))
		})
	}
}

func TestPackReadFS_RejectsWrites(t *testing.T) {
	root := t.TempDir()
	view, err := newPackReadFS(osfs.New(root))
	require.NoError(t, err)
	_, err = view.OpenFile("x", os.O_RDWR|os.O_CREATE, 0600)
	assert.ErrorIs(t, err, billy.ErrReadOnly)
	_, err = view.Create("x")
	assert.ErrorIs(t, err, billy.ErrReadOnly)
	_, err = view.TempFile("", "x")
	assert.ErrorIs(t, err, billy.ErrReadOnly)
	assert.ErrorIs(t, view.Remove("x"), billy.ErrReadOnly)
	assert.ErrorIs(t, view.Rename("x", "y"), billy.ErrReadOnly)
	assert.ErrorIs(t, view.MkdirAll("x", 0700), billy.ErrReadOnly)
	assert.ErrorIs(t, view.Symlink("x", "y"), billy.ErrReadOnly)
	assert.Equal(t, billy.ReadCapability|billy.SeekCapability, view.Capabilities())
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestCommittedContent_BrokenIndexNamesCause(t *testing.T) {
	root := gitRepoWithCommit(t, "file.txt", "committed\n")
	dir := filepath.Join(root, ".git", "objects", "pack")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.pack"), []byte("broken"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.idx"), []byte("short"), 0600))
	before := dotGitSnapshot(t, root)
	_, err := CommittedBlobs(root)
	require.ErrorIs(t, err, ErrUnsupportedGitState)
	assert.ErrorContains(t, err, "broken.idx")
	assert.Equal(t, before, dotGitSnapshot(t, root))
}
