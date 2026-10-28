//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record is exactly the line the Linux user-path leg compares against.
// Refs: MGIT-230.8
func TestWriteShadowRecord_IsOneLineNamingTheDirectoryAndTheMountPoint(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rec")
	require.NoError(t, writeShadowRecord(f, "/home", "/home/vimal/w"))
	got, err := os.ReadFile(f) //nolint:gosec // G304: test-only; the test's own temp file
	require.NoError(t, err)
	assert.Equal(t, "dir=/home path=/home/vimal/w\n", string(got))
	info, err := os.Stat(f)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "a command running as another user must be able to read it")
}

func TestShadowRecordFile_IsUnderTmp(t *testing.T) {
	assert.Equal(t, "/tmp/.mgit-mount-point-shadowed", shadowRecordFile)
}
