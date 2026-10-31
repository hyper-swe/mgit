//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// Inspection must show retained bytes, without admitting them for export.
func TestShowInspection_CLI(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	cli := filepath.Join(t.TempDir(), "mgit")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", cli, "./cmd/mgit") //nolint:gosec // G204: fixed build target and test-owned output
	build.Dir = filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "change.txt"), []byte("base bytes\n"), 0600))
	liveLandCommand(t, root, cli, "init")
	liveLandCommand(t, root, cli, "work", "wt", "--task-id", "SHOW-1")
	native := filepath.Join(root, "wt")
	repo, err := gitstore.OpenLinked(native, filepath.Join(root, ".mgit"), model.TaskBranchName("SHOW-1"), time.Now)
	require.NoError(t, err)
	defer func() { require.NoError(t, repo.Close()) }()
	idx, err := index.New(filepath.Join(root, ".mgit", "index.db"), time.Now)
	require.NoError(t, err)
	defer func() { require.NoError(t, idx.Close()) }()
	ctx := context.Background()
	base, err := repo.Head()
	require.NoError(t, err)
	var first string
	for _, state := range []string{"host", "pending", "legacy"} {
		contents := state + " inspected bytes\n"
		require.NoError(t, os.WriteFile(filepath.Join(native, "change.txt"), []byte(contents), 0600))
		liveLandCommand(t, native, cli, "commit", "-a", "-m", "inspect "+state)
		hash, err := repo.Head()
		require.NoError(t, err)
		if state == "host" {
			first = hash
		} else {
			rec := index.TaskCommitInsert{TaskID: "AUDIT-" + state, CommitHash: hash, ContentHash: "retained"}
			if state == "pending" {
				rec.Origin = index.OriginLand
			}
			require.NoError(t, idx.AppendTaskCommit(ctx, rec))
		}
		t.Run(state, func(t *testing.T) {
			before, err := idx.GetContentSnapshot(ctx, "AUDIT-"+state)
			require.NoError(t, err)
			for _, flags := range [][]string{nil, {"--stat"}} {
				shown := liveLandCommand(t, native, cli, append([]string{"show", hash}, flags...)...)
				require.Contains(t, shown, "change.txt")
				if len(flags) == 0 {
					require.Contains(t, shown, "+"+contents)
				}
			}
			after, err := idx.GetContentSnapshot(ctx, "AUDIT-"+state)
			require.NoError(t, err)
			require.Equal(t, before, after, "inspection must not create admission")
			if state != "host" {
				_, err := liveLandCommandResult(t, native, cli, "diff", "--from", first, "--to", hash)
				require.Error(t, err, "inspection must not bypass the explicit-hash content gate")
			}
		})
	}
	t.Run("healthy-unindexed-base-remedy", func(t *testing.T) {
		for _, pair := range [][2]string{{base, first}, {first, base}} {
			refusal, err := liveLandCommandResult(t, native, cli, "diff", "--from", pair[0], "--to", pair[1])
			require.Error(t, err)
			require.Contains(t, refusal, "not an admitted task commit")
			require.Contains(t, refusal, "--task-id")
			require.NotContains(t, refusal, "RECOVERY-1")
			require.NotContains(t, refusal, "new empty directory")
		}
		// A clean first task can still diff against its pinned, unindexed base.
		patch := liveLandCommand(t, native, cli, "diff", "--task-id", "SHOW-1")
		require.Contains(t, patch, "+legacy inspected bytes")
	})
	t.Run("healthy-unindexed-apply-remedy", func(t *testing.T) {
		before, err := repo.Head()
		require.NoError(t, err)
		bytesBefore, err := os.ReadFile(filepath.Join(native, "change.txt")) //nolint:gosec // G304: fixed path in this test-owned fixture
		require.NoError(t, err)
		for name, args := range map[string][]string{
			"restore-file": {"restore", "change.txt", "--commit", base},
			"restore-all":  {"restore", "--all", "--commit", base},
			"cherry-pick":  {"cherry-pick", base},
			"rollback":     {"rollback", "--task-id", "SHOW-1", "--commit", base},
		} {
			t.Run(name, func(t *testing.T) {
				refusal, err := liveLandCommandResult(t, native, cli, args...)
				require.Error(t, err)
				require.Contains(t, refusal, "not an admitted task commit")
				require.Contains(t, refusal, "locally admitted task commit hash")
				require.NotContains(t, refusal, "RECOVERY-1")
				require.NotContains(t, refusal, "new empty directory")
				after, err := repo.Head()
				require.NoError(t, err)
				require.Equal(t, before, after)
				bytesAfter, err := os.ReadFile(filepath.Join(native, "change.txt")) //nolint:gosec // G304: fixed path in this test-owned fixture
				require.NoError(t, err)
				require.Equal(t, bytesBefore, bytesAfter)
			})
		}
	})
	t.Run("unreadable-tree-is-loud", func(t *testing.T) {
		// A readable commit header does not imply its tree object is available.
		hash, err := repo.WriteRawObject(plumbing.CommitObject, []byte(fmt.Sprintf("tree 1111111111111111111111111111111111111111\nparent %s\nauthor Test <test@example.com> 1 +0000\ncommitter Test <test@example.com> 1 +0000\n\nUnreadable tree\n", first)))
		require.NoError(t, err)
		shown, err := liveLandCommandResult(t, native, cli, "show", hash)
		require.Error(t, err, "a failed diff must not become successful metadata-only output")
		require.Contains(t, shown, "show diff")
		require.Contains(t, shown, "tree")
	})
}
