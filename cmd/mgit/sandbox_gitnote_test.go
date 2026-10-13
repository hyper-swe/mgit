package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// The launch request carries the flag: on by default (the daemon is not
// told to skip the repo), off with --no-guest-repo.
func TestLaunchRequestCarriesTheGuestRepoChoice(t *testing.T) {
	img := "img@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--no-guest-repo"}, true},
	} {
		fc := &fakeSandboxClient{}
		args := append([]string{"launch", "--task", "MGIT-2", "--worktree", "/w", "--image", img}, tc.args...)
		_, err := runSandbox(okConnect(fc), args...)
		require.NoError(t, err)
		require.NotNil(t, fc.launched)
		assert.Equal(t, tc.want, fc.launched.NoGuestRepo, "args %v", tc.args)
	}
}
