package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// MGIT-131 at the process boundary an agent actually observes. The reported
// shape: build a binary the way the verification checklist says to, then run
// the instructed commit. Both staging paths are pinned here — `mgit add` and
// `mgit commit -a` — because a guard on one of them is not a guard.
// Refs: FR-2, FR-8.3, MGIT-131

// writeArtifact writes a file of n bytes at rel under dir, creating parents.
func writeArtifact(t *testing.T, dir, rel string, n int64) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, make([]byte, n), 0o600))
}

// seedRepo initializes a repo with one real commit so later commits have a
// parent tree to differ from.
func seedRepo(t *testing.T, bin, repo string) {
	t.Helper()
	require.NoError(t, runMgitTest(t, bin, repo, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runMgitTest(t, bin, repo, "add", "seed.txt"))
	require.NoError(t, runMgitTest(t, bin, repo, "commit", "--task-id", "MGIT-131", "-m", "seed"))
}

// assertRefusal checks the refusal an agent reads: it names the offending
// path, its size, the limit, and how to proceed anyway.
func assertRefusal(t *testing.T, out, path, size string) {
	t.Helper()
	assert.Contains(t, out, path, "the refusal must name the file")
	assert.Contains(t, out, size, "the refusal must state the file's size")
	assert.Contains(t, out, "limit", "the refusal must state the limit it broke")
	assert.Contains(t, out, "--allow-large", "the override must be discoverable from the refusal")
	assert.Contains(t, out, "limits.max_staged_file_mb", "the config key must be discoverable too")
}

func TestCommit_OversizedFile_RefusedByBothStagingPaths(t *testing.T) {
	bin := buildMgitTestBinary(t)

	// One byte over the shipped 5 MiB default — no config tweak, so this is the
	// behavior a contributor gets out of the box.
	const oversized = (5 << 20) + 1

	t.Run("commit_dash_a_refuses_and_records_nothing", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		require.NoError(t, os.WriteFile(filepath.Join(repo, "fix.go"), []byte("package fix\n"), 0o600))
		writeArtifact(t, repo, "build/mgit", oversized)

		logBefore, _ := runMgitTestExit(t, bin, repo, "log", "--task-id", "MGIT-131")
		out, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--task-id", "MGIT-131", "-m", "step")

		assert.NotZero(t, code, "an oversized stage must not report success: %s", out)
		assertRefusal(t, out, "build/mgit", "5.0 MB")

		logAfter, _ := runMgitTestExit(t, bin, repo, "log", "--task-id", "MGIT-131")
		assert.Equal(t, logBefore, logAfter, "a refused commit must not advance the branch")
	})

	t.Run("add_by_name_refuses", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "build/mgit", oversized)

		out, code := runMgitTestExit(t, bin, repo, "add", "build/mgit")

		assert.NotZero(t, code, "an oversized stage must not report success: %s", out)
		assertRefusal(t, out, "build/mgit", "5.0 MB")
	})

	t.Run("add_dash_A_refuses", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "build/mgit", oversized)

		out, code := runMgitTestExit(t, bin, repo, "add", "-A")

		assert.NotZero(t, code, "an oversized stage must not report success: %s", out)
		assertRefusal(t, out, "build/mgit", "5.0 MB")
	})
}

func TestCommit_JustUnderLimit_Unaffected(t *testing.T) {
	// The other half of a usable threshold: normal work must not even notice
	// the guard. One byte under the limit still commits.
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()
	seedRepo(t, bin, repo)
	writeArtifact(t, repo, "assets/fixture.bin", (5<<20)-1)

	out, code := runMgitTestExit(t, bin, repo,
		"commit", "-a", "--task-id", "MGIT-131", "-m", "large but allowed fixture")

	assert.Zero(t, code, "a file under the limit must commit normally: %s", out)

	show, code := runMgitTestExit(t, bin, repo, "status")
	require.Zero(t, code)
	assert.NotContains(t, show, "assets/fixture.bin", "the fixture should be committed, not pending")
}

func TestCommit_OversizedFile_EscapeHatchesWork(t *testing.T) {
	bin := buildMgitTestBinary(t)
	const oversized = (5 << 20) + 1

	t.Run("allow_large_flag_named_in_the_refusal", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "model.bin", oversized)

		refusal, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--task-id", "MGIT-131", "-m", "blocked")
		require.NotZero(t, code)
		require.Contains(t, refusal, "--allow-large")

		// Use exactly what the refusal told the caller to use.
		out, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--allow-large", "--task-id", "MGIT-131", "-m", "deliberately large")
		assert.Zero(t, code, "the documented override must work: %s", out)
	})

	t.Run("add_allow_large_flag", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "model.bin", oversized)

		out, code := runMgitTestExit(t, bin, repo, "add", "--allow-large", "model.bin")
		assert.Zero(t, code, "the documented override must work on add too: %s", out)
		assert.Contains(t, out, "Staged: model.bin")
	})

	t.Run("config_key_named_in_the_refusal", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "model.bin", oversized)

		refusal, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--task-id", "MGIT-131", "-m", "blocked")
		require.NotZero(t, code)
		require.Contains(t, refusal, "mgit config set limits.max_staged_file_mb")

		require.NoError(t, runMgitTest(t, bin, repo,
			"config", "set", "limits.max_staged_file_mb", "64"))
		out, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--task-id", "MGIT-131", "-m", "limit raised deliberately")
		assert.Zero(t, code, "raising the configured limit must take effect: %s", out)
	})

	t.Run("config_zero_disables_the_guard", func(t *testing.T) {
		repo := t.TempDir()
		seedRepo(t, bin, repo)
		writeArtifact(t, repo, "model.bin", oversized)

		require.NoError(t, runMgitTest(t, bin, repo,
			"config", "set", "limits.max_staged_file_mb", "0"))
		out, code := runMgitTestExit(t, bin, repo,
			"commit", "-a", "--task-id", "MGIT-131", "-m", "guard off")
		assert.Zero(t, code, "0 must disable the guard as the refusal states: %s", out)
	})
}

// TestCommit_GitignoredBuildOutput_NeverReachesTheGuard pins the FIRST line of
// defense: the reason the tripwire should almost never fire is that build
// output is ignored, so `commit -a` does not see it at all. This walks the
// exact route the ticket reported — the verification checklist's build command,
// then the instructed commit. Refs: MGIT-131
func TestCommit_GitignoredBuildOutput_NeverReachesTheGuard(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()

	// This project's .gitignore stanza, verbatim in shape.
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"),
		[]byte("/build/*\n!/build/darwin/\n/build/darwin/*\n!/build/darwin/README.md\n*.test\n"), 0o600))
	seedRepo(t, bin, repo)

	writeArtifact(t, repo, "build/mgit", (5<<20)+1)
	writeArtifact(t, repo, "internal/store/git/git.test", (5<<20)+1)
	writeArtifact(t, repo, "build/darwin/README.md", 32)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "real.go"), []byte("package real\n"), 0o600))

	status, code := runMgitTestExit(t, bin, repo, "status")
	require.Zero(t, code)
	assert.NotContains(t, status, "build/mgit", "ignored build output must not show as pending")
	assert.NotContains(t, status, "git.test", "ignored test binary must not show as pending")

	out, code := runMgitTestExit(t, bin, repo,
		"commit", "-a", "--task-id", "MGIT-131", "-m", "source change only")
	require.Zero(t, code, "the guard must never even be reached: %s", out)

	show, code := runMgitTestExit(t, bin, repo, "show", strings.TrimSpace(commitIDFrom(t, out)))
	require.Zero(t, code)
	assert.NotContains(t, show, "build/mgit", "the binary must not be in the recorded commit")
	// The un-ignored tracked file under build/ still commits: the negation is
	// not decoration, the Makefile reads build/darwin for codesign inputs.
	assert.Contains(t, show, "build/darwin/README.md")
}

func TestAddError_SizeRefusal_PassedThroughUnwrapped(t *testing.T) {
	// The refusal is self-contained; anything else gets the caller's context.
	size := oversizedTestError()
	assert.Same(t, size, addError("add build/mgit", size),
		"the size refusal must not be re-wrapped")

	other := errors.New("pathspec did not match any files: nope")
	wrapped := addError("add nope", other)
	assert.ErrorIs(t, wrapped, other)
	assert.Contains(t, wrapped.Error(), "add nope: ")
}

func TestStagingLimit_AllowLarge_DisablesTheGuard(t *testing.T) {
	app := &App{StagedFileLimit: 5 << 20}
	assert.Equal(t, int64(5<<20), stagingLimit(app, false))
	assert.Equal(t, int64(0), stagingLimit(app, true), "--allow-large must disable the guard")
}

// oversizedTestError returns an error carrying the size sentinel, as the store
// layer produces it.
func oversizedTestError() error {
	return fmt.Errorf("%w: build/mgit is 20.7 MB (limit 5.0 MB)", model.ErrFileTooLarge)
}

// commitIDFrom extracts the short commit id mgit prints as "[abcd1234] msg".
func commitIDFrom(t *testing.T, out string) string {
	t.Helper()
	open := strings.Index(out, "[")
	closeIdx := strings.Index(out, "]")
	require.Greater(t, closeIdx, open, "commit output %q has no [id] prefix", out)
	return out[open+1 : closeIdx]
}
