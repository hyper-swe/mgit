//go:build unix

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The MGIT-89 /etc shadow re-creates /etc in a tmpfs as root, and must give
// every entry its source owner back: the composed base is owner-only (passwd
// 0600, directories 0750), so a root-owned copy is unreadable to the guest's
// non-root exec identity. That identity then has no name, and cannot read
// resolv.conf or the CA bundle either. chownLike read the owner through a
// *unix.Stat_t assertion, but os.FileInfo.Sys() is a *syscall.Stat_t, a
// different type, so the assertion never held and nothing was ever chowned.
// Refs: MGIT-230.3, MGIT-89, MGIT-151
func TestOwnerOf_ReadsTheOwnerOfARealFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	info, err := os.Lstat(p)
	require.NoError(t, err)

	uid, gid, ok := ownerOf(info)
	require.True(t, ok, "the owner of a file os.Lstat returned must be readable (Sys() is %T)", info.Sys())
	assert.Equal(t, os.Getuid(), uid)
	assert.Equal(t, os.Getgid(), gid)
}

// A FileInfo that carries no platform stat (a fake, or another platform's)
// is "owner unknown", never uid 0.
func TestOwnerOf_NoPlatformStat_IsUnknown(t *testing.T) {
	_, _, ok := ownerOf(statlessInfo{})
	assert.False(t, ok)
}

// statlessInfo is a FileInfo with no platform stat behind it.
type statlessInfo struct{}

func (statlessInfo) Name() string       { return "x" }
func (statlessInfo) Size() int64        { return 0 }
func (statlessInfo) Mode() fs.FileMode  { return 0o644 }
func (statlessInfo) ModTime() time.Time { return time.Time{} }
func (statlessInfo) IsDir() bool        { return false }
func (statlessInfo) Sys() any           { return nil }
