package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Launching on a tree that has git metadata says, at launch, that the guest
// gets the files and not the repository, and what to do about it. A tree
// with none says nothing. Refs: FEAT-6.114
func TestWriteGitViewNote(t *testing.T) {
	linked := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /host/path\n"), 0o600))
	var out bytes.Buffer
	writeGitViewNote(&out, linked, "X-1")
	assert.Contains(t, out.String(), "mgit sandbox exec --task-id X-1")
	assert.Contains(t, out.String(), "git init")

	var none bytes.Buffer
	writeGitViewNote(&none, t.TempDir(), "X-1")
	assert.Empty(t, none.String())
}
