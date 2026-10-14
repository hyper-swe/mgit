package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortBaseFor points the fixed short fallback at a scratch dir under /tmp and
// returns it; the real fallback is /tmp itself. Refs: MGIT-279
func shortBaseFor(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "sf")
	require.NoError(t, err)
	old := shortRuntimeBase
	shortRuntimeBase = root
	t.Cleanup(func() {
		shortRuntimeBase = old
		// a scratch tree this test made: files first, then the empty dirs
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = os.Remove(p)
			}
			return nil
		})
		for i := 0; i < 6; i++ {
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					_ = os.Remove(p) // only succeeds on an empty dir
				}
				return nil
			})
		}
	})
	return root
}

func longTmpdir(t *testing.T, n int) string {
	t.Helper()
	b := filepath.Join(t.TempDir(), strings.Repeat("x", n))
	require.NoError(t, os.MkdirAll(b, 0o700))
	return b
}

// With a network allowlist the libkrun backend binds the per-sandbox socket at
// <work dir>/<8-char id>/prox.sock, and the vsock sockets beside it. The work
// dir came from TMPDIR, so a long TMPDIR made those paths unbindable and the
// guest never started. The sandbox's runtime dir now falls back to a short
// fixed per-user dir when the worst per-sandbox socket would not fit.
// Refs: MGIT-279
func TestResolveSandboxPaths_ALongTmpdirStillLeavesRoomForEveryPerSandboxSocket(t *testing.T) {
	repo := newRepo(t)
	short := shortBaseFor(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	for _, n := range []int{20, 60, 80, 120, 200} {
		t.Run(fmt.Sprintf("tmpdir_%d", n), func(t *testing.T) {
			t.Setenv("TMPDIR", longTmpdir(t, n))
			p, err := resolveSandboxPaths(repo)
			require.NoError(t, err, "a long TMPDIR must not stop the sandbox from starting")
			for _, name := range []string{"prox.sock", "deny.sock", "v65535.sock", "c.sock"} {
				sock := filepath.Join(p.workDir, "abcdefgh", name)
				assert.LessOrEqual(t, len(sock), maxSocketPathBytes, "%s (%d bytes) must fit", sock, len(sock))
			}
			assert.LessOrEqual(t, len(p.socket), maxSocketPathBytes)
			if n >= 60 {
				assert.True(t, strings.HasPrefix(p.workDir, short), "a long TMPDIR falls back to the short fixed dir, got %s", p.workDir)
			}
			// The platform's own answer: the worst per-sandbox socket really binds.
			dir := filepath.Join(p.workDir, "abcdefgh")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			ln, err := net.Listen("unix", filepath.Join(dir, "v65535.sock"))
			require.NoError(t, err, "the kernel binds the worst per-sandbox socket")
			_ = ln.Close() // opened only to prove the bind
		})
	}
}

// A TMPDIR that is short enough is used as it always was, so existing sandboxes
// and daemon listings do not move. Refs: MGIT-279
func TestRuntimeBase_AShortTmpdirIsUsedAsIs(t *testing.T) {
	short := shortBaseFor(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/tmp")
	assert.Equal(t, os.TempDir(), runtimeBase())
	assert.NotEqual(t, short, runtimeBase())
}

// The fallback lives in a shared directory, so it is owner-only and never a
// symlink someone else planted. Refs: MGIT-279
func TestResolveSandboxPaths_TheFallbackDirIsOwnerOnlyAndNotASymlink(t *testing.T) {
	repo := newRepo(t)
	short := shortBaseFor(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", longTmpdir(t, 120))
	mine := filepath.Join(short, fmt.Sprintf("mgit-%d", os.Getuid()))

	require.NoError(t, os.Mkdir(mine, 0o755)) // ours, but open to others
	require.NoError(t, os.Chmod(mine, 0o755))
	_, err := resolveSandboxPaths(repo)
	require.NoError(t, err)
	fi, err := os.Stat(mine)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm(), "the per-user dir is tightened to owner-only")

	other := t.TempDir()
	require.NoError(t, os.Rename(mine, mine+".old"))
	require.NoError(t, os.Symlink(other, mine))
	_, err = resolveSandboxPaths(repo)
	require.Error(t, err, "a symlink where the per-user dir belongs is refused")
	assert.Contains(t, err.Error(), "symlink")
}

// Daemons started under a long TMPDIR live in the short fixed dir, so the host
// listing must look there as well, but only at a per-user dir that is really
// this user's. Refs: MGIT-279
func TestHostRuntimeBases_IncludeTheShortDirOnlyWhenItIsOurs(t *testing.T) {
	short := shortBaseFor(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", longTmpdir(t, 120))
	mine := filepath.Join(short, fmt.Sprintf("mgit-%d", os.Getuid()))

	assert.Equal(t, []string{configuredRuntimeBase()}, hostRuntimeBases(), "no per-user dir yet, nothing to look at")

	require.NoError(t, os.Mkdir(mine, 0o700))
	assert.Equal(t, []string{configuredRuntimeBase(), short}, hostRuntimeBases())

	require.NoError(t, os.Remove(mine))
	require.NoError(t, os.Symlink(t.TempDir(), mine))
	assert.Equal(t, []string{configuredRuntimeBase()}, hostRuntimeBases(), "a symlink is not trusted")
}
