package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guest is given its own one-commit repository by default, so the
// launch has nothing to say about git. With --no-guest-repo, a tree that
// has git metadata is told that the guest gets the files and not a
// repository, and what to do about it. A tree with none says nothing.
// Refs: FEAT-6.114, FEAT-6.114.1
func TestWriteGitViewNote(t *testing.T) {
	linked := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /host/path\n"), 0o600))

	var off bytes.Buffer
	writeGitViewNote(&off, linked, "X-1", true)
	assert.Contains(t, off.String(), "mgit sandbox exec --task-id X-1")
	assert.Contains(t, off.String(), "git init")
	assert.Contains(t, off.String(), "--no-guest-repo")

	var byDefault bytes.Buffer
	writeGitViewNote(&byDefault, linked, "X-1", false)
	assert.Empty(t, byDefault.String(), "the note is for the flag-off case only")

	var none bytes.Buffer
	writeGitViewNote(&none, t.TempDir(), "X-1", true)
	assert.Empty(t, none.String())
}

func TestLaunchHasAFlagThatTurnsTheGuestRepoOff(t *testing.T) {
	cmd := sandboxLaunchCmd(nil)
	f := cmd.Flags().Lookup("no-guest-repo")
	require.NotNil(t, f, "the flag exists")
	assert.Equal(t, "false", f.DefValue, "the guest repo is on by default")
}
