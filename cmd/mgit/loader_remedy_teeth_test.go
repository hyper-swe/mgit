package main

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// asRPATH rewrites a fixture's DT_RUNPATH entry to DT_RPATH in place. Go's -r
// writes RUNPATH, while the release daemon carries RPATH (it must win over
// LD_LIBRARY_PATH), so this is how a test gets the tag the release ships.
func asRPATH(t *testing.T, path string) {
	t.Helper()
	f, err := elf.Open(path)
	require.NoError(t, err)
	dyn := f.Section(".dynamic")
	require.NotNil(t, dyn, "the fixture is dynamic")
	off, size := int64(dyn.Offset), int64(dyn.Size)
	require.NoError(t, f.Close())
	//nolint:gosec // G304: a fixture this test built
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	patched := false
	for p := off; p+16 <= off+size; p += 16 {
		if elf.DynTag(binary.LittleEndian.Uint64(b[p:])) == elf.DT_RUNPATH {
			binary.LittleEndian.PutUint64(b[p:], uint64(elf.DT_RPATH))
			patched = true
		}
	}
	require.True(t, patched, "the fixture carried a DT_RUNPATH to rewrite")
	require.NoError(t, os.WriteFile(path, b, 0o700)) //nolint:gosec // G306: the fixture daemon stays executable
}

// The release daemon carries DT_RPATH, and an install may reach it through a
// symlink (a PATH entry, a Homebrew link). The loader expands $ORIGIN from
// the REAL binary's directory, so the remedy must name the real lib/, found
// through the RPATH tag. The first fixture carried RUNPATH and no symlink, so
// reading only RUNPATH, or expanding $ORIGIN from the link, both passed.
// Refs: MGIT-230.4.1, MGIT-230.4
func TestMissingLibraryRemedy_OnLinux_ReadsDTRPATHThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := runPathDaemon(t, root, filepath.Join("real", "bin", "mgit-sandboxd"), "$ORIGIN/lib:$ORIGIN/../lib/mgit")
	asRPATH(t, real)
	link := filepath.Join(root, "links", "mgit-sandboxd")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	require.NoError(t, os.Symlink(real, link))

	got := missingLibraryRemedy("libkrun.so.1", link, "linux")
	assert.Contains(t, got, filepath.Join(root, "real", "bin", "lib"), "the real binary's lib/, from DT_RPATH")
	assert.Contains(t, got, filepath.Join(root, "real", "lib", "mgit"))
	assert.NotContains(t, got, filepath.Join(root, "links", "lib"), "not the symlink's directory")
	assert.NotContains(t, got, "carries no run path", "RPATH is a run path too")
}

// The activation error hands the daemon it started to the remedy. Passing
// "" instead drops the sentence about where that daemon looked, and no test
// noticed. This drives the real activation with a daemon on PATH that dies
// the way ld.so makes a daemon die, and requires the remedy to have looked at
// that daemon.
// Refs: MGIT-230.4.1, MGIT-230.4
func TestSandboxConnect_AFailedActivationNamesTheDaemonItStarted(t *testing.T) {
	repo := newRepo(t)
	short, err := os.MkdirTemp("/tmp", "act")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) }) // a scratch runtime base this test made
	t.Setenv("XDG_RUNTIME_DIR", short)
	bin := t.TempDir()
	daemon := filepath.Join(bin, "mgit-sandboxd")
	//nolint:gosec // G306: the fake daemon must be executable
	require.NoError(t, os.WriteFile(daemon, []byte("#!/bin/sh\n"+
		"echo 'mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file: No such file or directory' >&2\n"+
		"exit 127\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = sandboxConnectFor(context.Background(), repo)
	require.Error(t, err, "a daemon that cannot load never serves")
	assert.Contains(t, err.Error(), "libkrun.so.1", "the loader's words are relayed")
	// The fake is a script, so its run path cannot be read. The remedy says
	// so, and it can say it only when it was handed the daemon that was
	// started: with "" the whole sentence about where it looked is dropped.
	assert.Contains(t, err.Error(), "could not read its run path",
		"the remedy looked at the daemon that was started")
}
