package gitref

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shared clones borrow objects through an absolute alternate path. Both stores
// must stay untouched, including when source objects live in maintenance packs.
// Refs: MGIT-294, MGIT-14
func TestCommittedContent_AbsoluteAlternates(t *testing.T) {
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "maintenance"
		}
		t.Run(name, func(t *testing.T) {
			source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
			if packed {
				alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
				alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
			}
			clone := filepath.Join(t.TempDir(), "shared")
			alternatesGit(t, source, "clone", "--shared", source, clone)
			alt, err := os.ReadFile(filepath.Join(clone, ".git", "objects", "info", "alternates")) //nolint:gosec // fixed fixture path in a test-owned shared clone
			require.NoError(t, err)
			require.True(t, filepath.IsAbs(strings.TrimSpace(string(alt))))
			require.Equal(t, "borrowed\n", alternatesGit(t, clone, "show", "HEAD:file.txt"))
			beforeSource, beforeClone := dotGitSnapshot(t, source), dotGitSnapshot(t, clone)
			blobs, err := CommittedBlobs(clone)
			require.NoError(t, err)
			assert.Equal(t, plumbing.ComputeHash(plumbing.BlobObject, []byte("borrowed\n")).String(), blobs["file.txt"])
			files, _, err := CommittedFiles(clone)
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, "borrowed\n", string(files[0].Content))
			assert.Equal(t, beforeSource, dotGitSnapshot(t, source))
			assert.Equal(t, beforeClone, dotGitSnapshot(t, clone))
		})
	}
}

// A missing borrowed store must name the failed path and a recovery step,
// rather than hiding the alternate failure as a generic missing object.
// Refs: MGIT-294
func TestCommittedContent_MissingAlternateNamesRecovery(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	missing := filepath.Join(t.TempDir(), "missing", "objects")
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(missing+"\n"), 0600))
	before := dotGitSnapshot(t, clone)
	_, err := CommittedBlobs(clone)
	require.ErrorIs(t, err, ErrUnsupportedGitState)
	assert.ErrorContains(t, err, missing)
	assert.ErrorContains(t, err, "restore")
	assert.Equal(t, before, dotGitSnapshot(t, clone))
}

func alternatesGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput() //nolint:gosec // test-owned roots and fixed verbs
	require.NoError(t, err, "%s", out)
	return string(out)
}

// Alternates can be chained; each borrowed object database must retain the
// indexed-pack view and absolute-path resolution. Refs: MGIT-294
func TestCommittedContent_ChainedAbsoluteAlternates(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
	alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	alternatesGit(t, source, "clone", "--shared", source, first)
	alternatesGit(t, first, "clone", "--shared", first, second)
	before := dotGitSnapshot(t, source)
	files, _, err := CommittedFiles(second)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, before, dotGitSnapshot(t, source))
}

// Denied reads are injected rather than relying on root/host permissions.
// Refs: MGIT-294
func TestReadOnlyStorage_UnreadableAlternatesNamesRecovery(t *testing.T) {
	root := t.TempDir()
	fs := &deniedAlternatesFS{Filesystem: osfs.New(root)}
	_, err := NewReadOnlyStorage(fs)
	require.Error(t, err)
	assert.ErrorContains(t, err, root)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.ErrorContains(t, err, "restore access")
}

type deniedAlternatesFS struct{ billy.Filesystem }

func (fs *deniedAlternatesFS) Open(path string) (billy.File, error) {
	if filepath.Clean(path) == filepath.Join("objects", "info", "alternates") {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(fs.Root(), path), Err: os.ErrPermission}
	}
	return fs.Filesystem.Open(path)
}

// Preserve an existing rooted relative alternate alongside absolute support.
// Refs: MGIT-294
func TestCommittedContent_RootedRelativeAlternate(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	borrowed := filepath.Join(clone, ".git", "borrowed")
	alternatesGit(t, source, "clone", "--no-hardlinks", source, borrowed)
	relative := filepath.ToSlash(filepath.Join("..", "borrowed", ".git", "objects"))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(relative+"\n"), 0600))
	require.Equal(t, "borrowed\n", alternatesGit(t, clone, "show", "HEAD:file.txt"))
	before := dotGitSnapshot(t, clone)
	files, _, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, before, dotGitSnapshot(t, clone))
}

// An inaccessible borrowed object directory must not be swallowed by fallback.
// Refs: MGIT-294
func TestCommittedContent_UnreadableAlternateStore(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	objects := filepath.Join(source, ".git", "objects")
	require.NoError(t, os.Chmod(objects, 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(objects, 0700)) }) //nolint:gosec // test-owned directory needs traversal restored for cleanup
	if _, err := os.ReadDir(objects); err == nil {
		t.Skip("NOT RUN: process can read mode-000 directories")
	}
	_, err := CommittedBlobs(clone)
	require.ErrorIs(t, err, ErrUnsupportedGitState)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.ErrorContains(t, err, objects)
	assert.ErrorContains(t, err, "restore access")
}

// Validate cycles before go-git's recursive fallback can loop on a missing
// object. This invokes construction only, never an unsafe cyclic lookup.
// Refs: MGIT-294
func TestReadOnlyStorage_CyclicAlternatesNamesRecovery(t *testing.T) {
	root := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	objects := filepath.Join(root, ".git", "objects")
	require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(objects+"\n"), 0600))
	before := dotGitSnapshot(t, root)
	_, err := NewReadOnlyStorage(osfs.New(filepath.Join(root, ".git")))
	require.Error(t, err)
	assert.ErrorContains(t, err, "cyclic alternate object store")
	assert.ErrorContains(t, err, objects)
	assert.ErrorContains(t, err, "restore access")
	assert.Equal(t, before, dotGitSnapshot(t, root))
}
