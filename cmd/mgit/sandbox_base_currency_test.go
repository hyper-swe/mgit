package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// baseFixture stores a base tree in this test's own cache whose composed-by
// marker names version ("" writes no marker), and returns its digest.
func baseFixture(t *testing.T, version string) string {
	t.Helper()
	cache, err := basecache.Open()
	require.NoError(t, err)
	dir, err := cache.Stage()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "sh"), []byte("#!/bin/sh\n# "+version+"\n"), 0o600))
	if version != "" {
		require.NoError(t, guestbase.WriteComposedBy(dir, version, ""))
	}
	entry, err := cache.Commit(dir, images.TreeDigest)
	require.NoError(t, err)
	return entry.Digest
}

// A STALE BASE IS SAID WHERE IT MATTERS, NOT ONLY IN DOCTOR (MGIT-224,
// MGIT-174's second bullet). A base composed by another mgit carries that
// release's guest binaries, so it silently lacks every guest-side change
// since. The note names both versions and the recompose command; a base
// that does not record its composer is UNKNOWN; a current base, or an image
// this CLI did not compose, says nothing. Refs: MGIT-224, MGIT-174
func TestBaseCurrencyNotice_NamesBothVersionsAndTheRecompose(t *testing.T) {
	t.Setenv(basecache.EnvRoot, t.TempDir())
	stale, unknown, current := baseFixture(t, "0.6.7"), baseFixture(t, ""), baseFixture(t, Version)
	cache, err := basecache.Open()
	require.NoError(t, err)

	notice := baseCurrencyNotice(cache, stale, Version)
	for _, want := range []string{"composed by mgit 0.6.7", "this is mgit " + Version, "mgit sandbox base from", "nothing was refused"} {
		assert.Contains(t, notice, want)
	}
	assert.Contains(t, baseCurrencyNotice(cache, unknown, Version), "UNKNOWN")
	assert.Empty(t, baseCurrencyNotice(cache, current, Version), "a current base says nothing")
	assert.Empty(t, baseCurrencyNotice(cache, "sha256:"+strings.Repeat("9", 64), Version), "an image this CLI did not compose says nothing")
}

func runSplit(t *testing.T, cmdOut func() (*bytes.Buffer, *bytes.Buffer, error)) (string, string) {
	t.Helper()
	stdout, stderr, err := cmdOut()
	require.NoError(t, err)
	return stdout.String(), stderr.String()
}

// The moments that matter: `sandbox status`, the launch, and the one exec
// that boots the VM. The note goes to stderr, so every verb's stdout is what
// it was, and it is said once per boot, not once per command: an exec into
// a sandbox that is already running says nothing. It never refuses and
// never changes an exit code. Refs: MGIT-224, MGIT-174
func TestStaleBase_IsWarnedAtStatusLaunchAndTheExecThatBoots(t *testing.T) {
	t.Setenv(basecache.EnvRoot, t.TempDir())
	stale := baseFixture(t, "0.6.7")
	const warning = "composed by mgit 0.6.7"
	sandbox := func(state, dir string) *model.SandboxInfo {
		return &model.SandboxInfo{ID: "01JSB", TaskID: "MGIT-224", State: state, WorktreePath: dir, ImageDigest: stale}
	}
	sandboxCmd := func(fake *fakeSandboxClient, args ...string) func() (*bytes.Buffer, *bytes.Buffer, error) {
		return func() (*bytes.Buffer, *bytes.Buffer, error) {
			cmd := newSandboxCmd(okConnect(fake))
			var o, e bytes.Buffer
			cmd.SetOut(&o)
			cmd.SetErr(&e)
			cmd.SetArgs(args)
			return &o, &e, cmd.Execute()
		}
	}
	runCmdWith := func(fake *fakeSandboxClient, dir string) func() (*bytes.Buffer, *bytes.Buffer, error) {
		return func() (*bytes.Buffer, *bytes.Buffer, error) {
			cmd := newRunCmd(okConnect(fake), func() (string, error) { return dir, nil })
			var o, e bytes.Buffer
			cmd.SetOut(&o)
			cmd.SetErr(&e)
			cmd.SetArgs([]string{"--", "id"})
			return &o, &e, cmd.Execute()
		}
	}
	dir := t.TempDir()

	out, errOut := runSplit(t, sandboxCmd(&fakeSandboxClient{statusInfo: sandbox(model.StateRunning, dir)}, "status", "MGIT-224"))
	assert.Equal(t, 1, strings.Count(errOut, warning), "status warns once: %q", errOut)
	assert.NotContains(t, out, warning, "stdout is unchanged")

	out, errOut = runSplit(t, sandboxCmd(&fakeSandboxClient{}, "launch", "--task-id", "MGIT-224", "--worktree", dir, "--image", "base@"+stale))
	assert.Equal(t, 1, strings.Count(errOut, warning), "the launch warns once: %q", errOut)
	assert.NotContains(t, out, warning)

	booting := &fakeSandboxClient{execStdout: "uid=501\n", listResult: []model.SandboxInfo{*sandbox(model.StateCreated, canonicalPath(dir))}}
	out, errOut = runSplit(t, runCmdWith(booting, dir))
	assert.Equal(t, 1, strings.Count(errOut, warning), "the exec that boots the VM warns once: %q", errOut)
	assert.Equal(t, "uid=501\n", out, "the command's own output is untouched")

	running := &fakeSandboxClient{execStdout: "uid=501\n", listResult: []model.SandboxInfo{*sandbox(model.StateRunning, canonicalPath(dir))}}
	_, errOut = runSplit(t, runCmdWith(running, dir))
	assert.NotContains(t, errOut, warning, "an exec into a running sandbox says nothing")

	out, errOut = runSplit(t, sandboxCmd(&fakeSandboxClient{execStdout: "uid=501\n", statusInfo: sandbox(model.StateCreated, dir)},
		"exec", "--task-id", "MGIT-224", "--", "id"))
	assert.Equal(t, 1, strings.Count(errOut, warning), "sandbox exec warns at the boot too: %q", errOut)
	assert.Equal(t, "uid=501\n", out)
}
