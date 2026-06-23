package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// faultStorer wraps a real go-git storer and injects faults on demand, so the
// store-failure error branches of mgit's plumbing (which are unreachable with a
// healthy store) can be exercised — a real safety scenario: mgit must fail
// loudly, never silently, when the object store cannot write. Refs: FR-12, NFR-5
type faultStorer struct {
	storage.Storer
	failSetObject bool
	failCAS       bool
}

func (f *faultStorer) SetEncodedObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	if f.failSetObject {
		return plumbing.ZeroHash, errors.New("injected: object store write failure")
	}
	return f.Storer.SetEncodedObject(o)
}

func (f *faultStorer) CheckAndSetReference(nw, old *plumbing.Reference) error {
	if f.failCAS {
		return errors.New("injected: reference update failure")
	}
	return f.Storer.CheckAndSetReference(nw, old)
}

// newFaultRepo builds a real, committable mgit Repository whose object store is
// a faultStorer (faults initially OFF). The returned *faultStorer toggles
// faults for the operation under test. Coexists with a real project .git.
func newFaultRepo(t *testing.T) (*Repository, *faultStorer) {
	t.Helper()
	tmp := t.TempDir()
	seedProjectGit(t, tmp)
	mgitPath := filepath.Join(tmp, mgitDirName)
	require.NoError(t, os.MkdirAll(mgitPath, 0o750))

	base := filesystem.NewStorage(osfs.New(mgitPath), cache.NewObjectLRUDefault())
	fs := &faultStorer{Storer: base}
	goRepo, err := gogit.Init(fs, nil)
	require.NoError(t, err)
	r := &Repository{root: tmp, repo: goRepo, clock: fixedClock()}
	require.NoError(t, r.createInitialCommit())
	return r, fs
}

// stageOneFile writes and stages a file so a subsequent commit has real work.
func stageOneFile(t *testing.T, repo *Repository, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), name), []byte(content), 0o600))
	require.NoError(t, NewWorktreeStore(repo).Add(context.Background(), name))
}

// TestCreateCommit_ObjectStoreWriteFault_Errors: when the object store cannot
// write (blob/tree/commit), CreateCommit fails rather than producing a partial
// or fake commit.
func TestCreateCommit_ObjectStoreWriteFault_Errors(t *testing.T) {
	repo, fs := newFaultRepo(t)
	cs := NewCommitStore(repo)
	stageOneFile(t, repo, "a.go", "package a\n")

	fs.failSetObject = true
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err := cs.CreateCommit(context.Background(), c)
	assert.Error(t, err, "a failed object-store write must fail the commit")
}

// TestCreateCommit_RefUpdateFault_Errors: when the branch ref CAS fails (e.g. a
// concurrent move), CreateCommit surfaces the ref-update error.
func TestCreateCommit_RefUpdateFault_Errors(t *testing.T) {
	repo, fs := newFaultRepo(t)
	cs := NewCommitStore(repo)
	stageOneFile(t, repo, "a.go", "package a\n")

	fs.failCAS = true
	c := makeTestModelCommit(t, "MGIT-1")
	c.FileDiffs = nil
	_, err := cs.CreateCommit(context.Background(), c)
	assert.Error(t, err, "a failed ref update must fail the commit")
}

// TestCreateMergeCommit_ObjectStoreWriteFault_Errors: a merge whose merged-tree
// write fails surfaces the error rather than advancing the ref to a bad commit.
func TestCreateMergeCommit_ObjectStoreWriteFault_Errors(t *testing.T) {
	repo, fs := newFaultRepo(t)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)
	ms := NewMergeStore(repo)
	ctx := context.Background()

	// main has a file; a source branch diverges with its own.
	stageOneFile(t, repo, "a.go", "package a\n")
	ca := makeTestModelCommit(t, "MGIT-1")
	ca.FileDiffs = nil
	base, err := cs.CreateCommit(ctx, ca)
	require.NoError(t, err)
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "feature", HeadCommit: base}))
	require.NoError(t, bs.SwitchBranch(ctx, "feature"))
	stageOneFile(t, repo, "b.go", "package b\n")
	cb := makeTestModelCommit(t, "MGIT-2")
	cb.FileDiffs = nil
	src, err := cs.CreateCommit(ctx, cb)
	require.NoError(t, err)
	require.NoError(t, bs.SwitchBranch(ctx, "main"))

	fs.failSetObject = true
	_, err = ms.CreateMergeCommit(ctx, "merge feature", src)
	assert.Error(t, err, "a failed merged-tree write must fail the merge")
}

// TestPlumbing_WriteHelpers_StoreFault_Errors covers the low-level plumbing
// error wraps directly: writeBlob, writeFlatTree, and writeCommit each surface a
// store write failure.
func TestPlumbing_WriteHelpers_StoreFault_Errors(t *testing.T) {
	fs := &faultStorer{Storer: memory.NewStorage(), failSetObject: true}

	_, err := writeBlob(fs, []byte("x"))
	assert.Error(t, err, "writeBlob must surface a store fault")

	_, err = writeFlatTree(fs, nil)
	assert.Error(t, err, "writeFlatTree must surface a store fault")

	_, err = writeCommit(fs, commitParams{tree: plumbing.ZeroHash})
	assert.Error(t, err, "writeCommit must surface a store fault")
}
