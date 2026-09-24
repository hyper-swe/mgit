package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// base/boots sets the daemon's VMM against the SHAPE of the base the repo
// registered, and the shape comes from the same resolve the daemon's boot
// uses: `sandbox base from` registers a directory, `sandbox image add` a
// kernel + rootfs image. Both are registered here the way a user does it, and
// read back. Refs: MGIT-230.4
func TestInspectBaseShape_ReadsTheRegisteredBasesShape(t *testing.T) {
	t.Run("a_composed_directory", func(t *testing.T) {
		srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh"})
		defer srv.Close()
		repo := newRepo(t)
		_, err := initTrustRoot(t, repo)
		require.NoError(t, err)
		out, err := runBase(t, repo, "from", ref, "--guest-bin-dir", fakeGuestBins(t), "--plain-http")
		require.NoError(t, err, "base from: %s", out)
		t.Chdir(repo)
		shape, err := inspectBaseShape()
		require.NoError(t, err)
		assert.Equal(t, defaultGuestBaseName, shape.Name)
		assert.True(t, shape.RootIsDir, "base from composes a directory: %+v", shape)
		assert.Empty(t, shape.KernelPath, "libkrunfw carries the kernel; the base names none")
	})
	t.Run("a_kernel_and_rootfs_image", func(t *testing.T) {
		repo := newRepo(t)
		_, err := runImage(t, repo, "init")
		require.NoError(t, err)
		kernel, rootfs := writeImageFiles(t)
		_, err = runImage(t, repo, "add", "--name", defaultGuestBaseName, "--kernel", kernel, "--rootfs", rootfs)
		require.NoError(t, err)
		t.Chdir(repo)
		shape, err := inspectBaseShape()
		require.NoError(t, err)
		assert.False(t, shape.RootIsDir, "the rootfs is an image file")
		assert.Equal(t, kernel, shape.KernelPath)
		assert.Equal(t, rootfs, shape.RootfsPath)
	})
	t.Run("nothing_registered", func(t *testing.T) {
		repo := newRepo(t)
		_, err := runImage(t, repo, "init")
		require.NoError(t, err)
		t.Chdir(repo)
		_, err = inspectBaseShape()
		require.Error(t, err, "no base is a cannot-tell, never a shape")
		assert.Contains(t, err.Error(), "no guest base registered")
	})
}
