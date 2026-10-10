package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Resync must not retain ignored Git-untracked metadata merely because an
// earlier foundation capture imported it. Git-tracked ignored files survive.
// Refs: MGIT-290, ADR-008 §3, FR-1.3b
func TestResync_IgnoredUntrackedCaptureOrders(t *testing.T) {
	for _, rule := range []string{"info_exclude", "nested_gitignore"} {
		for _, captureFirst := range []bool{false, true} {
			name := rule + "/exclude_first"
			if captureFirst {
				name = rule + "/capture_first"
			}
			t.Run(name, func(t *testing.T) {
				env, root := envOverRealGit(t)
				ctx := context.Background()
				gr, err := gogit.PlainOpen(root)
				require.NoError(t, err)
				gw, err := gr.Worktree()
				require.NoError(t, err)
				writeImportFixture(t, root, "metadata/keep.txt", "tracked\n")
				_, err = gw.Add("metadata/keep.txt")
				require.NoError(t, err)
				sig := &object.Signature{Name: "test", Email: "test@example.invalid", When: env.repo.Now()}
				_, err = gw.Commit("tracked metadata", &gogit.CommitOptions{Author: sig})
				require.NoError(t, err)
				writeImportFixture(t, root, "metadata/private.txt", "untracked\n")
				svc := NewSyncService(env.repo, env.wt, env.cs, "", fixedClock())
				if captureFirst {
					require.NoError(t, svc.EnsureSyncedForNewWorktree(ctx))
				}
				ignorePath, patterns := ".git/info/exclude", "metadata/*\n"
				if rule == "nested_gitignore" {
					ignorePath, patterns = "metadata/.gitignore", "*\n"
				}
				writeImportFixture(t, root, ignorePath, patterns)
				beforeGit := gitSnapshot(t, root)
				oldHead, err := env.repo.Head()
				require.NoError(t, err)
				require.NoError(t, svc.EnsureSyncedForNewWorktree(ctx))
				head, err := env.repo.Head()
				require.NoError(t, err)
				_, err = env.cs.GetFileFromCommit(ctx, head, "metadata/private.txt")
				assert.Error(t, err, "ignored untracked file must not remain in resynced base")
				keep, err := env.cs.GetFileFromCommit(ctx, head, "metadata/keep.txt")
				require.NoError(t, err)
				assert.Equal(t, "tracked\n", string(keep))
				data, err := os.ReadFile(filepath.Join(root, "metadata", "private.txt")) //nolint:gosec // test-owned fixture
				require.NoError(t, err)
				assert.Equal(t, "untracked\n", string(data), "resync must leave user's file on disk")
				assert.Equal(t, beforeGit, gitSnapshot(t, root))
				if captureFirst {
					prior, err := env.cs.GetFileFromCommit(ctx, oldHead, "metadata/private.txt")
					require.NoError(t, err, "resync must preserve prior commit objects")
					assert.Equal(t, "untracked\n", string(prior))
				}
				again, err := env.repo.Head()
				require.NoError(t, err)
				require.NoError(t, svc.EnsureSyncedForNewWorktree(ctx))
				unchanged, err := env.repo.Head()
				require.NoError(t, err)
				assert.Equal(t, again, unchanged, "repeat resync is idempotent")
			})
		}
	}
}

func writeImportFixture(t *testing.T, root, rel, data string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, []byte(data), 0o600))
}
