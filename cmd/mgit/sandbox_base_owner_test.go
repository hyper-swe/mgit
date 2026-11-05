package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capture contents and metadata without including signing-key contents in failures.
func baseOwnerSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s:%d", info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path) //nolint:gosec // G304: walk is confined to test-owned fixture roots
			if err != nil {
				return err
			}
			value += fmt.Sprintf(":%x", sha256.Sum256(data))
		}
		result[rel] = value
		return nil
	}))
	return result
}

func baseOwnerFixture(t *testing.T, kind string) (string, string) {
	t.Helper()
	repo, wt := t.TempDir(), t.TempDir()
	t.Setenv("MGIT_BASE_CACHE", t.TempDir())
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init", "--no-agent-files"))
	if kind == "linked" {
		require.NoError(t, os.MkdirAll(filepath.Join(wt, ".mgit"), 0o700))
		data, err := json.Marshal(map[string]string{"store": filepath.Join(repo, ".mgit"), "branch": "task/BASE-1", "task": "BASE-1"})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "worktree"), data, 0o600))
	} else {
		require.NoError(t, gitstore.WriteSandboxOwner(wt, gitstore.SandboxOwner{RepoRoot: repo, Task: "BASE-1"}))
	}
	return repo, wt
}

func TestBaseSetOwnerRefusal(t *testing.T) {
	for _, kind := range []string{"linked", "sandbox"} {
		for _, trust := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/trust=%t", kind, trust), func(t *testing.T) {
				repo, wt := baseOwnerFixture(t, kind)
				if trust {
					_, err := initTrustRoot(t, repo)
					require.NoError(t, err)
					_, err = runBase(t, repo, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t))
					require.NoError(t, err)
				}
				nested := filepath.Join(wt, "sub", "nested")
				require.NoError(t, os.MkdirAll(nested, 0o700))
				tree, bins := userspaceTree(t), fakeGuestBins(t)
				beforeRepo, beforeTree, beforeWT := baseOwnerSnapshot(t, repo), baseOwnerSnapshot(t, tree), baseOwnerSnapshot(t, wt)
				for _, cwd := range []string{wt, nested} {
					out, err := runBase(t, cwd, "set", tree, "--guest-bin-dir", bins, "--json")
					if assert.Error(t, err) {
						assert.Contains(t, err.Error(), canonicalPath(repo))
						assert.Contains(t, err.Error(), canonicalPath(wt))
						assert.Contains(t, err.Error(), "run")
						assert.Contains(t, err.Error(), "mgit sandbox base set")
					}
					assert.NotContains(t, out, "image_ref")
					assert.Equal(t, beforeRepo, baseOwnerSnapshot(t, repo), "owner must not change")
					assert.Equal(t, beforeTree, baseOwnerSnapshot(t, tree), "input tree must not change")
					assert.Equal(t, beforeWT, baseOwnerSnapshot(t, wt), "worktree must not change")
				}
			})
		}
	}
}

func TestBaseSetOwnerExplicit(t *testing.T) {
	for _, kind := range []string{"root", "subdirectory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			repo := newRepo(t)
			cwd := repo
			switch kind {
			case "subdirectory":
				cwd = filepath.Join(repo, "nested")
				require.NoError(t, os.Mkdir(cwd, 0o700))
			case "symlink":
				cwd = filepath.Join(t.TempDir(), "alias")
				require.NoError(t, os.Symlink(repo, cwd))
			}
			out, err := runBase(t, cwd, "set", userspaceTree(t), "--guest-bin-dir", fakeGuestBins(t), "--json")
			require.NoError(t, err)
			assert.Contains(t, out, "image_ref")
		})
	}
}

func TestBaseSetOwnerRealCLI(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo, wt := baseOwnerFixture(t, "sandbox")
	tree, bins := userspaceTree(t), fakeGuestBins(t)
	beforeRepo, beforeTree := baseOwnerSnapshot(t, repo), baseOwnerSnapshot(t, tree)
	run := func(cwd string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "sandbox", "base", "set", tree, "--guest-bin-dir", bins, "--json") //nolint:gosec // test-built executable and fixture paths
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run(wt)
	assert.Error(t, err, "worktree must refuse")
	assert.Contains(t, out, canonicalPath(repo))
	assert.Contains(t, out, "mgit sandbox base set")
	assert.NotContains(t, out, "image_ref")
	assert.Equal(t, beforeRepo, baseOwnerSnapshot(t, repo))
	assert.Equal(t, beforeTree, baseOwnerSnapshot(t, tree))
	out, err = run(repo)
	require.NoError(t, err, "%s", out)
	assert.Contains(t, out, "image_ref")
}

func TestBaseSetOwnerRefusalPrecedesInputValidation(t *testing.T) {
	repo, wt := baseOwnerFixture(t, "sandbox")
	before := baseOwnerSnapshot(t, repo)
	_, err := runBase(t, wt, "set", filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), canonicalPath(repo))
	assert.Contains(t, err.Error(), "run `mgit sandbox base set`")
	assert.Equal(t, before, baseOwnerSnapshot(t, repo))
}

func TestBaseSetOwnerInvalidMarkers(t *testing.T) {
	for _, kind := range []string{"malformed", "missing-owner"} {
		t.Run(kind, func(t *testing.T) {
			repo, wt := baseOwnerFixture(t, "sandbox")
			if kind == "malformed" {
				require.NoError(t, os.WriteFile(filepath.Join(wt, ".mgit", "worktree"), []byte("{"), 0o600))
			} else {
				require.NoError(t, gitstore.WriteSandboxOwner(wt, gitstore.SandboxOwner{RepoRoot: filepath.Join(t.TempDir(), "gone"), Task: "BASE-1"}))
			}
			tree := userspaceTree(t)
			beforeRepo, beforeTree := baseOwnerSnapshot(t, repo), baseOwnerSnapshot(t, tree)
			_, err := runBase(t, wt, "set", tree, "--guest-bin-dir", fakeGuestBins(t))
			require.Error(t, err)
			if kind == "malformed" {
				assert.Contains(t, err.Error(), "read worktree marker")
			} else {
				assert.Contains(t, err.Error(), "holds no mgit store")
			}
			assert.Equal(t, beforeRepo, baseOwnerSnapshot(t, repo))
			assert.Equal(t, beforeTree, baseOwnerSnapshot(t, tree))
		})
	}
}
