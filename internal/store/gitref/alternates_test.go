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
	storage, err := NewReadOnlyStorage(fs)
	require.NoError(t, err)
	_, err = storage.EncodedObject(plumbing.AnyObject, plumbing.ZeroHash)
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

// A missing-object search bounds cycles instead of entering go-git's recursive
// fallback; constructing storage does not eagerly inspect alternate stores.
// Refs: MGIT-294
func TestReadOnlyStorage_CyclicAlternatesNamesRecovery(t *testing.T) {
	root := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	objects := filepath.Join(root, ".git", "objects")
	require.NoError(t, os.MkdirAll(filepath.Join(objects, "info"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(objects, "info", "alternates"), []byte(objects+"\n"), 0600))
	before := dotGitSnapshot(t, root)
	storage, err := NewReadOnlyStorage(osfs.New(filepath.Join(root, ".git")))
	require.NoError(t, err)
	_, err = storage.EncodedObject(plumbing.AnyObject, plumbing.ZeroHash)
	require.Error(t, err)
	assert.ErrorContains(t, err, "cyclic alternate object store")
	assert.ErrorContains(t, err, objects)
	assert.ErrorContains(t, err, "restore access")
	assert.Equal(t, before, dotGitSnapshot(t, root))
}

// Repacking a shared clone makes it self-contained. A stale alternates line
// must not prevent reading objects already present locally. Refs: MGIT-294
func TestCommittedContent_RepackedSharedCloneMissingSource(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	alternatesGit(t, clone, "repack", "-a", "-d")
	require.NoError(t, os.RemoveAll(source)) // source is exclusively created by this test's t.TempDir
	require.Contains(t, alternatesGit(t, clone, "show", "HEAD:file.txt"), "borrowed\n")
	before := dotGitSnapshot(t, clone)
	blobs, err := CommittedBlobs(clone)
	require.NoError(t, err)
	assert.Contains(t, blobs, "file.txt")
	files, _, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, before, dotGitSnapshot(t, clone))
}

// Git relative alternates are relative to the object database, including paths
// outside the clone; they are not host-root paths. Refs: MGIT-294
func TestCommittedContent_RelativeAlternateOutsideClone(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	relative, err := filepath.Rel(filepath.Join(clone, ".git", "objects"), filepath.Join(source, ".git", "objects"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(relative+"\n"), 0600))
	require.Contains(t, alternatesGit(t, clone, "show", "HEAD:file.txt"), "borrowed\n")
	beforeSource, beforeClone := dotGitSnapshot(t, source), dotGitSnapshot(t, clone)
	files, _, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, beforeSource, dotGitSnapshot(t, source))
	assert.Equal(t, beforeClone, dotGitSnapshot(t, clone))
}

// One unavailable configured source must not hide a later readable source.
// Refs: MGIT-294
func TestCommittedContent_UnavailableThenReadableAlternate(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	missing := filepath.Join(t.TempDir(), "missing", "objects")
	borrowed := filepath.Join(source, ".git", "objects")
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(missing+"\n"+borrowed+"\n"), 0600))
	require.Contains(t, alternatesGit(t, clone, "show", "HEAD:file.txt"), "borrowed\n")
	beforeSource, beforeClone := dotGitSnapshot(t, source), dotGitSnapshot(t, clone)
	files, _, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, beforeSource, dotGitSnapshot(t, source))
	assert.Equal(t, beforeClone, dotGitSnapshot(t, clone))
}

// Cached borrowed pack indexes are shared across reads without a mutable-map
// race. Refs: MGIT-294
func TestReadOnlyStorage_ConcurrentBorrowedReads(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
	alternatesGit(t, source, "maintenance", "run", "--task=loose-objects")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	hash := plumbing.NewHash(strings.TrimSpace(alternatesGit(t, clone, "rev-parse", "HEAD")))
	storage, err := NewReadOnlyStorage(osfs.New(filepath.Join(clone, ".git")))
	require.NoError(t, err)
	results := make(chan error, 10)
	for range 10 {
		go func() { _, readErr := storage.EncodedObject(plumbing.CommitObject, hash); results <- readErr }()
	}
	failures := make([]error, 0, 10)
	for range 10 {
		failures = append(failures, <-results)
	}
	for _, readErr := range failures {
		require.NoError(t, readErr)
	}
	require.NoError(t, storage.HasEncodedObject(hash))
	size, err := storage.EncodedObjectSize(hash)
	require.NoError(t, err)
	assert.Positive(t, size)
}

// The alternate names an object directory, not a repository; its basename is
// not constrained to "objects". Refs: MGIT-294
func TestCommittedContent_AlternateObjectDirectoryName(t *testing.T) {
	source := gitRepoWithCommit(t, "file.txt", "borrowed\n")
	clone := filepath.Join(t.TempDir(), "shared")
	alternatesGit(t, source, "clone", "--shared", source, clone)
	original := filepath.Join(source, ".git", "objects")
	renamed := filepath.Join(source, ".git", "borrowed-objects")
	require.NoError(t, os.Rename(original, renamed))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "objects", "info", "alternates"), []byte(renamed+"\n"), 0600))
	require.Contains(t, alternatesGit(t, clone, "show", "HEAD:file.txt"), "borrowed\n")
	beforeSource, beforeClone := dotGitSnapshot(t, source), dotGitSnapshot(t, clone)
	files, _, err := CommittedFiles(clone)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "borrowed\n", string(files[0].Content))
	assert.Equal(t, beforeSource, dotGitSnapshot(t, source))
	assert.Equal(t, beforeClone, dotGitSnapshot(t, clone))
}
