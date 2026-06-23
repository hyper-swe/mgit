package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Filesystem + object-store fault coverage for the store's remaining
// read/write error branches. Refs: FR-12, NFR-5

func TestBlobContent_MissingBlob_Errors(t *testing.T) {
	repo := initTestRepo(t)
	_, err := repo.blobContent(plumbing.ZeroHash)
	assert.Error(t, err, "reading a missing blob must error")
}

func TestWriteEntryToDir_MissingBlob_Errors(t *testing.T) {
	repo := initTestRepo(t)
	err := repo.writeEntryToDir(t.TempDir(), "x.go", blobEntry{hash: plumbing.ZeroHash, mode: filemode.Regular})
	assert.Error(t, err, "materializing an entry whose blob is missing must error")
}

func TestBlobHashOfWorkingFile_Missing_Errors(t *testing.T) {
	repo := initTestRepo(t)
	_, err := repo.blobHashOfWorkingFile("does-not-exist.go")
	assert.Error(t, err, "hashing a non-existent working file must error")
}

func TestWriteRawObject_StoreFault_Errors(t *testing.T) {
	repo, fs := newFaultRepo(t)
	fs.failSetObject = true
	_, err := repo.WriteRawObject(plumbing.BlobObject, []byte("x"))
	assert.Error(t, err, "WriteRawObject must surface a store write fault")
}

func TestHead_ReferenceFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetReference = true
	_, err := repo.Head()
	assert.Error(t, err, "Head must surface a reference read fault")
}

func TestCreateInitialCommit_StoreFault_Errors(t *testing.T) {
	tmp := t.TempDir()
	seedProjectGit(t, tmp)
	mgitPath := filepath.Join(tmp, mgitDirName)
	require.NoError(t, os.MkdirAll(mgitPath, 0o750))
	base := filesystem.NewStorage(osfs.New(mgitPath), cache.NewObjectLRUDefault())
	fs := &faultStorer{Storer: base, failSetObject: true}
	goRepo, err := gogit.Init(fs, nil)
	require.NoError(t, err)
	r := &Repository{root: tmp, repo: goRepo, clock: fixedClock()}
	assert.Error(t, r.createInitialCommit(), "initial commit must fail when the store can't write")
}

func TestSaveStaging_ReadOnlyDir_Errors(t *testing.T) {
	repo := initTestRepo(t)
	// Make .mgit read-only so writing staging.json.tmp fails.
	require.NoError(t, os.Chmod(repo.MgitDir(), 0o500))       //nolint:gosec // dir needs exec bit; read-only is the fault under test
	t.Cleanup(func() { _ = os.Chmod(repo.MgitDir(), 0o750) }) //nolint:gosec // restore dir perms
	err := repo.stagePaths([]string{"a.go"})
	assert.Error(t, err, "staging under a read-only .mgit must fail")
}

func TestClearStaging_PathIsNonEmptyDir_Errors(t *testing.T) {
	repo := initTestRepo(t)
	// Make staging path a non-empty directory so os.Remove fails (not NotExist).
	sp := filepath.Join(repo.MgitDir(), stagingFileName)
	require.NoError(t, os.MkdirAll(filepath.Join(sp, "child"), 0o750))
	err := repo.clearStaging()
	assert.Error(t, err, "clearing a non-empty dir staging path must error")
}

func TestAddAll_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, _ := faultRepoWithCommit(t)
	fs.failGetObject = true
	err := NewWorktreeStore(repo).Add(context.Background(), ".")
	assert.Error(t, err, "add -A must fail when status can't be computed")
}

func TestMergeBase_And_IsAncestor_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failGetObject = true
	ms := NewMergeStore(repo)
	_, err := ms.MergeBase(context.Background(), h, h)
	assert.Error(t, err)
	_, err = ms.IsAncestor(context.Background(), h, h)
	assert.Error(t, err)
}

func TestFastForward_ObjectReadFault_Errors(t *testing.T) {
	repo, fs, h := faultRepoWithCommit(t)
	fs.failGetObject = true
	err := NewMergeStore(repo).FastForward(context.Background(), "task/MGIT-1", h)
	assert.Error(t, err, "fast-forward materialize must surface a read fault")
}

func TestMaterializeBranchTo_MkdirError(t *testing.T) {
	repo, _, _ := faultRepoWithCommit(t)
	// A regular file where the destination dir must be created blocks MkdirAll.
	dest := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.WriteFile(dest, []byte("x"), 0o600))
	err := NewWorktreeStore(repo).MaterializeBranchTo(context.Background(), "task/MGIT-1", filepath.Join(dest, "sub"))
	assert.Error(t, err, "materialize into a path under a file must fail")
}
