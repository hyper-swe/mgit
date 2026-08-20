package basecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cross-filesystem half of adoption cannot be reached by a rename in a
// test — one temp dir is one filesystem — so the fallback is exercised
// directly. It is not dead code: a repository on a project volume and a cache
// under the user's home are routinely on different devices. Refs: MGIT-147

// fixedDigest hashes a tree the way the production digester does for the
// purposes of these tests: any stable function of the contents will do, since
// what is under test is the MOVE, not the hash.
func fixedDigest(dir string) (string, error) {
	hasher := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hasher, "%s|%v\n", rel, d.IsDir())
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path) //nolint:gosec // test-owned tree
			if err != nil {
				return err
			}
			hasher.Write(data)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func TestAdoptByCopy_CopiesTheTreeAndRemovesTheOriginal(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)

	legacy := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(filepath.Join(legacy, "sbin"), 0o750))
	//nolint:gosec // G306: the guest supervisor must be executable
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "sbin", "mgit-guest"), []byte("ELF"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "etc-hosts"), []byte("127.0.0.1"), 0o600))
	require.NoError(t, os.Symlink("sbin/mgit-guest", filepath.Join(legacy, "init")))

	digest, err := fixedDigest(legacy)
	require.NoError(t, err)
	path, err := cache.Path(digest)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))

	entry, err := cache.adoptByCopy(legacy, digest, Entry{Digest: digest, Path: path})
	require.NoError(t, err)

	assert.Equal(t, path, entry.Path)
	assert.False(t, entry.Deduplicated, "a copy that published is not a dedupe")
	assert.NoDirExists(t, legacy, "the original must be gone once its bytes are cached")

	// The executable bit survives: the tree digest covers it, so losing it
	// would change the base's identity and break the pin.
	info, err := os.Stat(filepath.Join(path, "sbin", "mgit-guest"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o111, "the copy dropped the executable bit")

	// Symlinks are copied verbatim, dangling or not — a guest base is full of
	// links that only resolve once it is the guest's root.
	target, err := os.Readlink(filepath.Join(path, "init"))
	require.NoError(t, err)
	assert.Equal(t, "sbin/mgit-guest", target)
}

func TestAdoptByCopy_WhenTheDigestIsAlreadyCached_DropsTheOriginal(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)

	staging, err := cache.Stage()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "f"), []byte("x"), 0o600))
	published, err := cache.Commit(staging, fixedDigest)
	require.NoError(t, err)

	legacy := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(legacy, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "f"), []byte("x"), 0o600))

	entry, err := cache.adoptByCopy(legacy, published.Digest,
		Entry{Digest: published.Digest, Path: published.Path})
	require.NoError(t, err)
	assert.True(t, entry.Deduplicated)
	assert.NoDirExists(t, legacy)
}

func TestCopyTree_WithAnUnreadableSource_Fails(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o000))
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000 file")
	}
	err := copyTree(src, t.TempDir())
	require.Error(t, err, "an unreadable source must fail loudly, not produce a short tree")
}

func TestOpen_UsesTheDefaultRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvRoot, dir)

	cache, err := Open()
	require.NoError(t, err)
	assert.Equal(t, dir, cache.Root())
}

func TestOpen_WithAnUnusableDefaultRoot_Fails(t *testing.T) {
	t.Setenv(EnvRoot, "not/absolute")
	_, err := Open()
	require.Error(t, err)
}

func TestCommit_WithNoDigester_IsRefused(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)
	_, err = cache.Commit(t.TempDir(), nil)
	require.Error(t, err)
	_, err = cache.Adopt(t.TempDir(), nil)
	require.Error(t, err)
}

func TestCommit_WhenTheDigesterFails_ReportsIt(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)
	staging, err := cache.Stage()
	require.NoError(t, err)

	_, err = cache.Commit(staging, func(string) (string, error) {
		return "", fmt.Errorf("tree walk exploded")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tree walk exploded")
}

func TestCommit_WithADigestThatIsNotADigest_IsRefused(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)
	staging, err := cache.Stage()
	require.NoError(t, err)

	_, err = cache.Commit(staging, func(string) (string, error) { return "banana", nil })
	require.Error(t, err, "a digester returning nonsense must never become a cache path")
}

// The defensive I/O paths, exercised rather than assumed: a cache root that
// cannot be written must fail loudly at the point of failure, since every one
// of these guards a multi-gigabyte operation. Refs: MGIT-147

// readOnlyDir returns a directory nothing may be created in.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	//nolint:gosec // G302: a DIRECTORY that must stay traversable but unwritable is the point of this fixture
	require.NoError(t, os.Chmod(dir, 0o500))
	//nolint:gosec // G302: restored so t.TempDir cleanup can remove it
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

func TestStage_WhenTheCacheRootCannotBeCreated_Fails(t *testing.T) {
	cache, err := New(filepath.Join(readOnlyDir(t), "bases"))
	require.NoError(t, err)
	_, err = cache.Stage()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging")
}

func TestDiscard_WhenTheTreeCannotBeRemoved_Fails(t *testing.T) {
	parent := t.TempDir()
	victim := filepath.Join(parent, "tree")
	require.NoError(t, os.MkdirAll(victim, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "f"), []byte("x"), 0o600))
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	//nolint:gosec // G302: an unwritable DIRECTORY is the fixture
	require.NoError(t, os.Chmod(victim, 0o500))
	//nolint:gosec // G302: restored for cleanup
	t.Cleanup(func() { _ = os.Chmod(victim, 0o700) })

	cache, err := New(t.TempDir())
	require.NoError(t, err)
	require.Error(t, cache.Discard(victim))
}

func TestCommit_WhenTheEntriesDirectoryCannotBeCreated_Fails(t *testing.T) {
	root := t.TempDir()
	cache, err := New(root)
	require.NoError(t, err)
	staging, err := cache.Stage()
	require.NoError(t, err)
	// Wedge the entries directory: a FILE where the sha256/ directory belongs.
	require.NoError(t, os.WriteFile(filepath.Join(root, entriesDir), []byte("x"), 0o600))

	_, err = cache.Commit(staging, fixedDigest)
	require.Error(t, err)
}

func TestPruneStaging_WhenAStagingTreeCannotBeRemoved_Fails(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)
	staging, err := cache.Stage()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "f"), []byte("x"), 0o600))
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	//nolint:gosec // G302: an unwritable DIRECTORY is the fixture
	require.NoError(t, os.Chmod(staging, 0o500))
	//nolint:gosec // G302: restored for cleanup
	t.Cleanup(func() { _ = os.Chmod(staging, 0o700) })
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(staging, old, old))

	_, err = cache.PruneStaging(time.Now(), time.Hour)
	require.Error(t, err)
}

func TestCopyTree_WhenTheDestinationCannotBeWritten_Fails(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "f"), []byte("x"), 0o600))

	err := copyTree(src, filepath.Join(readOnlyDir(t), "dst"))
	require.Error(t, err)
}

func TestAdopt_WhenTheTreeIsMissing_Fails(t *testing.T) {
	cache, err := New(t.TempDir())
	require.NoError(t, err)
	_, err = cache.Adopt(filepath.Join(t.TempDir(), "gone"), fixedDigest)
	require.Error(t, err)
}
