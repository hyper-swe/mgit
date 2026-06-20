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

	require.NoError(t, buildWorktreeImage(context.Background(), mkfs, wt, img, 64))

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
	require.NoError(t, buildWorktreeImage(context.Background(), &fakeMkfs{}, wt, img, 0))
	fi, err := os.Stat(img)
	require.NoError(t, err)
	assert.Equal(t, int64(defaultWorktreeImageMB)<<20, fi.Size())
}

// TestBuildWorktreeImage_Rejections covers the fail-closed guards.
func TestBuildWorktreeImage_Rejections(t *testing.T) {
	img := filepath.Join(t.TempDir(), worktreeImageName)
	t.Run("relative_worktree", func(t *testing.T) {
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, "rel/wt", img, 64)
		assert.Error(t, err)
	})
	t.Run("worktree_absent", func(t *testing.T) {
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, filepath.Join(t.TempDir(), "nope"), img, 64)
		assert.Error(t, err)
	})
	t.Run("worktree_is_a_file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "afile")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
		err := buildWorktreeImage(context.Background(), &fakeMkfs{}, f, img, 64)
		assert.Error(t, err)
	})
}

// TestBuildWorktreeImage_MkfsFailure_RemovesPartialImage verifies an
// mke2fs failure leaves no partial image behind (clean fail-closed).
func TestBuildWorktreeImage_MkfsFailure_RemovesPartialImage(t *testing.T) {
	wt := worktreeWith(t)
	img := filepath.Join(t.TempDir(), worktreeImageName)
	err := buildWorktreeImage(context.Background(), &fakeMkfs{err: assert.AnError}, wt, img, 64)
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
	err := buildWorktreeImage(context.Background(), &fakeMkfs{}, wt, img, 64)
	assert.Error(t, err)
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
			err := buildWorktreeImage(context.Background(), mkfs, wt, filepath.Join(t.TempDir(), "img"), 64)
			require.Error(t, err)
			assert.Zero(t, mkfs.calls, "mke2fs is not run when sizing the image fails")
		})
	}
}
