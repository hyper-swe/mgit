package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// LAUNCH MUST USE THE BASE THE USER ALREADY REGISTERED.
//
// A digest is not something a person chooses — it is the output of composing a
// base. Making `--image` mandatory would mean copying a 64-hex string out of
// one command's output into the next, every time, and getting it wrong would
// be indistinguishable from tampering. Refs: MGIT-61.15, FR-17.17

func TestSandboxLaunch_WithNoImageFlag_UsesTheRegisteredBase(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	out, err := runBase(t, repo, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t))
	require.NoError(t, err, "base set: %s", out)

	fc := &fakeSandboxClient{}
	t.Chdir(repo)
	_, err = runSandbox(okConnect(fc), "launch", "--task", "MGIT-2", "--worktree", "/w")

	require.NoError(t, err, "a registered base must be enough to launch")
	require.NotNil(t, fc.launched)

	// Exactly the pinned reference, not merely something base-shaped: the
	// digest launch boots is the one images.lock signed, and it travels into
	// the append-only launch record as the attestation of what ran.
	want, err := images.PinnedRef(filepath.Join(repo, ".mgit", "sandbox"), "base")
	require.NoError(t, err)
	assert.Equal(t, want, fc.launched.ImageRef)
	assert.Contains(t, want, "sha256:")
}

func TestSandboxLaunch_WithNoBaseRegistered_FailsClosedNamingTheRemedy(t *testing.T) {
	// The first thing a new user does is launch a sandbox, and at that moment
	// there is no base. That error is the whole first-run experience: it must
	// name the ONE command that fixes it, not describe a missing flag.
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	fc := &fakeSandboxClient{}
	t.Chdir(repo)
	_, err = runSandbox(okConnect(fc), "launch", "--task", "MGIT-2", "--worktree", "/w")

	require.Error(t, err, "launching with no base must fail closed, never guess an image")
	assert.Contains(t, err.Error(), "mgit sandbox base from",
		"the remedy must be one runnable command, got %q", err)
	assert.Nil(t, fc.launched, "no launch may be sent when there is nothing to boot")
}

func TestSandboxLaunch_WithAnExplicitImage_DoesNotConsultTheBase(t *testing.T) {
	// An explicit --image still wins: pinning a specific image is how a user
	// runs something other than this repo's base.
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	out, err := runBase(t, repo, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t))
	require.NoError(t, err, "base set: %s", out)

	explicit := "img@sha256:" + strings.Repeat("a", 64)
	fc := &fakeSandboxClient{}
	t.Chdir(repo)
	_, err = runSandbox(okConnect(fc), "launch",
		"--task", "MGIT-2", "--worktree", "/w", "--image", explicit)

	require.NoError(t, err)
	require.NotNil(t, fc.launched)
	assert.Equal(t, explicit, fc.launched.ImageRef, "an explicit --image must win over the base")
}

// TestWorkSetup_SandboxWithNoImage_UsesTheRegisteredBase pins the same rule on
// the entry point agents actually use. `mgit work --sandbox` is the first
// command a new user runs; making it demand a digest would make the product's
// front door the hardest part of it. Refs: MGIT-61.15, MGIT-34
func TestWorkSetup_SandboxWithNoImage_UsesTheRegisteredBase(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	out, err := runBase(t, repo, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t))
	require.NoError(t, err, "base set: %s", out)

	fc := &fakeSandboxClient{}
	t.Chdir(repo)
	out, wt, err := runWorkSetup(t, &fakeWorktreeAdder{}, workOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-7.9", LaunchSandbox: true,
	}, okConnect(fc))

	require.NoError(t, err)
	require.NotNil(t, wt)
	require.NotNil(t, fc.launched, "the registered base is enough to launch: %s", out)
	assert.True(t, strings.HasPrefix(fc.launched.ImageRef, "base@sha256:"),
		"work must boot the pinned base, got %q", fc.launched.ImageRef)
}

func TestWorkSetup_SandboxWithNoBase_ReportsTheRemedyAndKeepsTheWorktree(t *testing.T) {
	// The worktree and agent wiring are useful on their own, so a missing base
	// degrades the sandbox leg rather than failing the command — the same
	// contract an unavailable daemon already has.
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)

	fc := &fakeSandboxClient{}
	t.Chdir(repo)
	out, wt, err := runWorkSetup(t, &fakeWorktreeAdder{}, workOptions{
		Path: filepath.Join(t.TempDir(), "wt"), TaskID: "MGIT-7.9", LaunchSandbox: true,
	}, okConnect(fc))

	require.NoError(t, err, "a missing base must not destroy the worktree flow")
	require.NotNil(t, wt)
	assert.Nil(t, fc.launched, "nothing may be booted when there is no base")
	assert.Contains(t, out, "mgit sandbox base from", "the remedy must be printed, got %q", out)
}
