package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMkfs records the args it was asked to run and can fail on demand.
type fakeMkfs struct {
	gotArgs []string
	calls   int
	err     error
}

func (f *fakeMkfs) run(_ context.Context, args ...string) error {
	f.calls++
	f.gotArgs = args
	return f.err
}

// worktreeWith creates a worktree dir containing one file and returns it.
func worktreeWith(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))
	return wt
}

// TestBuildWorktreeImage_PreSizesAndInvokesMke2fs verifies the image is
// pre-sized as a sparse file and mke2fs is invoked to populate it from the
// worktree (the -d source) into the image. Refs: MGIT-11.6.4
func TestBuildWorktreeImage_PreSizesAndInvokesMke2fs(t *testing.T) {
	wt := worktreeWith(t)
	img := filepath.Join(t.TempDir(), worktreeImageName)
	mkfs := &fakeMkfs{}

	require.NoError(t, buildWorktreeImage(context.Background(), mkfs, wt, img, "", 64))

	fi, err := os.Stat(img)
	require.NoError(t, err)
	assert.Equal(t, int64(64)<<20, fi.Size(), "image is pre-sized to the requested MB")
	require.Equal(t, 1, mkfs.calls)
	assert.Equal(t, []string{"-F", "-q", "-t", "ext4", "-d", wt, img}, mkfs.gotArgs,
		"mke2fs populates the image from the worktree with -d (rootless)")
}

// TestBuildWorktreeImage_DefaultSize verifies a non-positive size falls
// back to the default.
func TestBuildWorktreeImage_DefaultSize(t *testing.T) {
	wt := worktreeWith(t)
	img := filepath.Join(t.TempDir(), worktreeImageName)
	require.NoError(t, buildWorktreeImage(context.Background(), &fakeMkfs{}, wt, img, "", 0))
	fi, err := os.Stat(img)
	require.NoError(t, err)
	assert.Equal(t, int64(defaultWorktreeImageMB)<<20, fi.Size())
}

// TestBuildWorktreeImage_Rejections covers the fail-closed guards.
func TestBuildWorktreeImage_Rejections(t *testing.T) {
	img := filepath.Join(t.TempDir(), worktreeImageName)
	t.Run("relative_worktree", func(t *testing.T) {
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, "rel/wt", img, "", 64)
		assert.Error(t, err)
	})
	t.Run("worktree_absent", func(t *testing.T) {
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, filepath.Join(t.TempDir(), "nope"), img, "", 64)
		assert.Error(t, err)
	})
	t.Run("worktree_is_a_file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "afile")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, f, img, "", 64)
		assert.Error(t, err)
	})
}

// TestBuildWorktreeImage_MkfsFailure_RemovesPartialImage verifies an
// mke2fs failure leaves no partial image behind (clean fail-closed).
func TestBuildWorktreeImage_MkfsFailure_RemovesPartialImage(t *testing.T) {
	wt := worktreeWith(t)
	img := filepath.Join(t.TempDir(), worktreeImageName)
	err := buildWorktreeImage(context.Background(), &fakeMkfs{err: assert.AnError}, wt, img, "", 64)
	require.Error(t, err)
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "a failed build removes the partial image")
}

// TestBuildWorktreeImage_ExistingImage_Rejected verifies O_EXCL: a stale
// image at the target path is not silently reused.
func TestBuildWorktreeImage_ExistingImage_Rejected(t *testing.T) {
	wt := worktreeWith(t)
	img := filepath.Join(t.TempDir(), worktreeImageName)
	require.NoError(t, os.WriteFile(img, []byte("stale"), 0o600))
	err := buildWorktreeImage(context.Background(), &fakeMkfs{}, wt, img, "", 64)
	assert.Error(t, err)
}

// snapshotMkfs records, at mke2fs time, the relative paths present in the -d
// source dir — the staging tree is removed by buildWorktreeImage's defer once
// it returns, so the assertion must read it while mke2fs is "running".
type snapshotMkfs struct {
	srcDir string
	have   map[string]bool // worktree-relative paths (slash) present in the staging tree
	calls  int
}

func (s *snapshotMkfs) run(_ context.Context, args ...string) error {
	s.calls++
	// args = [-F -q -t ext4 -d <srcDir> <img>]; the -d source is index 5.
	s.srcDir = args[5]
	s.have = map[string]bool{}
	_ = filepath.WalkDir(s.srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.srcDir, path)
		if rel != "." {
			s.have[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return nil
}

// privateStoreWith creates a fake private .mgit store dir with one marker file.
func privateStoreWith(t *testing.T) string {
	t.Helper()
	priv := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(priv, "objects"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(priv, "HEAD"), []byte("ref: refs/heads/task/x\n"), 0o600))
	return priv
}

// TestBuildWorktreeImage_PrivateStore_PacksWorktreePlusPrivateMgit verifies the
// SEC-03 delivery: the staging tree packed into the image holds the worktree
// files plus the private store at .mgit, and excludes any in-worktree store.
func TestBuildWorktreeImage_PrivateStore_PacksWorktreePlusPrivateMgit(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "pkg"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "pkg", "x.go"), []byte("package pkg"), 0o600))
	// An in-worktree store that must NOT be packed (a clone's history, F-A).
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".mgit", "objects"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "LEAK"), []byte("host history"), 0o600))

	priv := privateStoreWith(t)
	snap := &snapshotMkfs{}
	img := filepath.Join(t.TempDir(), worktreeImageName)
	require.NoError(t, buildWorktreeImage(context.Background(), snap, wt, img, priv, 64))

	require.Equal(t, 1, snap.calls)
	assert.True(t, snap.have["main.go"], "worktree file packed")
	assert.True(t, snap.have["pkg/x.go"], "nested worktree file packed")
	assert.True(t, snap.have[".mgit/HEAD"], "private store laid in at .mgit")
	assert.False(t, snap.have[".mgit/LEAK"], "the in-worktree store's content is NOT packed")
	// The staging tree is cleaned up after the build returns (no residue).
	assert.NoDirExists(t, filepath.Join(filepath.Dir(img), stagingDirName))
}

// TestBuildWorktreeImage_PrivateStore_RejectsEscapingSymlink proves the SEC-03
// delivery rejects a worktree symlink whose target escapes the worktree
// (finding F-A/NEW-2), fails closed, and leaves no image or staging residue.
func TestBuildWorktreeImage_PrivateStore_RejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("host secret"), 0o600))
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "ok.txt"), []byte("ok"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(wt, "escape")))

	priv := privateStoreWith(t)
	snap := &snapshotMkfs{}
	img := filepath.Join(t.TempDir(), worktreeImageName)
	err := buildWorktreeImage(context.Background(), snap, wt, img, priv, 64)
	require.ErrorIs(t, err, errSymlinkEscape)
	assert.Zero(t, snap.calls, "mke2fs is never run when a symlink escapes")
	_, statErr := os.Stat(img)
	assert.True(t, os.IsNotExist(statErr), "no image is left behind on a fail-closed escape")
	assert.NoDirExists(t, filepath.Join(filepath.Dir(img), stagingDirName))
}

// TestBuildWorktreeImage_PrivateStore_PreservesInWorktreeSymlink verifies an
// IN-worktree symlink (target stays inside) is preserved, not rejected.
func TestBuildWorktreeImage_PrivateStore_PreservesInWorktreeSymlink(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wt, "real.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(wt, "link.txt"))) // relative, in-tree

	priv := privateStoreWith(t)
	snap := &snapshotMkfs{}
	img := filepath.Join(t.TempDir(), worktreeImageName)
	require.NoError(t, buildWorktreeImage(context.Background(), snap, wt, img, priv, 64))
	assert.True(t, snap.have["link.txt"], "an in-worktree symlink is preserved")
}

// faultyImageFile fails Truncate and/or Close to exercise the fail-closed
// sizing/close cleanup paths.
type faultyImageFile struct{ truncErr, closeErr error }

func (f faultyImageFile) Truncate(int64) error { return f.truncErr }
func (f faultyImageFile) Close() error         { return f.closeErr }

// TestBuildWorktreeImage_SizingFailures verifies a Truncate or Close
// failure on the image file fails closed and does not invoke mke2fs.
func TestBuildWorktreeImage_SizingFailures(t *testing.T) {
	wt := worktreeWith(t)
	orig := createImageFile
	t.Cleanup(func() { createImageFile = orig })

	for name, fault := range map[string]faultyImageFile{
		"truncate_fails": {truncErr: assert.AnError},
		"close_fails":    {closeErr: assert.AnError},
	} {
		t.Run(name, func(t *testing.T) {
			createImageFile = func(string) (imageFile, error) { return fault, nil }
			mkfs := &fakeMkfs{}
			err := buildWorktreeImage(context.Background(), mkfs, wt, filepath.Join(t.TempDir(), "img"), "", 64)
			require.Error(t, err)
			assert.Zero(t, mkfs.calls, "mke2fs is not run when sizing the image fails")
		})
	}
}
