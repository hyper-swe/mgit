package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// From a directory whose .mgit held only agent files, doctor exited 1 with
// nothing on either stream (0.6.5 included): its own SilenceErrors plus main's
// bare exit. A verdict with no words is the phantom door. R-H300 rule 4:
// nothing is silent. Refs: MGIT-196
func TestDoctor_OpenFailure_IsPrintedNotSilent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".mgit", "shims"), 0o750))
	t.Chdir(dir)

	out, err := runDoctor(t, unreachable(errors.New("no daemon")))

	require.Error(t, err)
	assert.Contains(t, out, "holds no store")
	assert.Contains(t, out, "mgit sandbox launch")
}

// A launch-decorated directory that names its owner: doctor opens the owning
// repository and asks its guest rows about the recorded task, the way
// `mgit run` routes from there. Refs: MGIT-196
func TestDoctor_OwnerPointer_OpensTheOwningRepositoryAndAsksItsTask(t *testing.T) {
	repo := projectWithGit(t)
	owned := filepath.Join(t.TempDir(), "owned")
	require.NoError(t, gitstore.WriteSandboxOwner(owned, gitstore.SandboxOwner{RepoRoot: repo, Task: "T-OWN"}))
	t.Chdir(owned)
	fc := &fakeSandboxClient{execStdout: "sha256sum\ndrop_caches\n127.0.0.1\tlocalhost\n", repoRoot: repo, socket: filepath.FromSlash("/run/d.sock")}

	out, err := runDoctor(t, connecting(fc))

	require.NoError(t, err)
	assert.Equal(t, "T-OWN", fc.execTask, "the guest probe must ask the recorded task")
	assert.Contains(t, out, "ok    guest/localhost")
}

// A directory inside the repository that a registered sandbox covers, with no
// marker and no owner file: doctor resolves the task from the daemon's own
// bindings, exactly as `mgit run` does, instead of "not bound". Refs: MGIT-196
func TestDoctor_DirectoryCoveredByARegisteredSandbox_AsksThatTask(t *testing.T) {
	repo := projectWithGit(t)
	sub := filepath.Join(repo, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	t.Chdir(sub)
	fc := &fakeSandboxClient{execStdout: "sha256sum\ndrop_caches\n127.0.0.1\tlocalhost\n", repoRoot: repo, socket: filepath.FromSlash("/run/d.sock"),
		listResult: []model.SandboxInfo{{ID: "s1", TaskID: "T-SUB", WorktreePath: canonicalPath(sub), State: model.StateCreated}}}

	out, err := runDoctor(t, connecting(fc))

	require.NoError(t, err)
	assert.Equal(t, "T-SUB", fc.execTask)
	assert.Contains(t, out, "ok    guest/localhost")
}

// From a repository root no sandbox covers, doctor's guest rows say what
// `mgit run` says there: which daemon was asked and what it holds — the same
// stated reason, so the two views cannot disagree. Refs: MGIT-196
func TestDoctor_UnboundDirectory_SaysWhatRunSays(t *testing.T) {
	repo := projectWithGit(t)
	elsewhere := filepath.FromSlash("/elsewhere/wt")
	fc := &fakeSandboxClient{repoRoot: repo, socket: filepath.FromSlash("/run/d.sock"),
		listResult: []model.SandboxInfo{{ID: "s1", TaskID: "T-9", WorktreePath: elsewhere, State: model.StateCreated}}}

	out, err := runDoctor(t, connecting(fc))

	require.NoError(t, err, "a check that could not run never fails the gate")
	assert.Contains(t, out, "no sandbox bound for")
	assert.Contains(t, out, "the daemon of repository "+repo)
	assert.Contains(t, out, "T-9 at "+elsewhere+" (created)")
	assert.Empty(t, fc.execTask, "nothing was asked of a guest")
}
