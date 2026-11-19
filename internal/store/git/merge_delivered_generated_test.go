package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/require"
)

// A recorded generated doc (AGENTS.md, CLAUDE.md) whose host copy differs from
// the baseline only inside mgit's own generated block must not stop a land:
// mgit rewrote that block itself when the worktree was made and when the
// sandbox launched. Any difference outside the block still refuses, and a path
// that was never recorded as generated keeps the old behavior. Refs: HSWE-45.2
const (
	docOutside  = "# Project notes\n\nwritten by the user\n"
	docOldBlock = "\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nold posture\n<!-- END mgit-sandbox -->\n"
	docNewBlock = "\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nnew posture\n<!-- END mgit-sandbox -->\n"
)

func TestFastForwardDelivered_GeneratedDocDrift(t *testing.T) {
	cases := []struct {
		name      string
		generated bool
		host      string
		lands     bool
	}{
		{"block-only drift lands", true, docOutside + docNewBlock, true},
		{"edit outside the block refuses", true, docOutside + "an extra user line\n" + docNewBlock, false},
		{"path not recorded as generated refuses", false, docOutside + docNewBlock, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initTestRepo(t)
			ctx := context.Background()
			base := createCommitWithFile(t, repo, "AGENTS.md", docOutside+docOldBlock, "HSWE-452")
			const branch = "task/HSWE-452"
			require.NoError(t, NewBranchStore(repo).CreateBranch(ctx, &model.Branch{Name: branch, HeadCommit: base}))
			tip := createCommitWithFile(t, repo, "new.txt", "new\n", "HSWE-452")
			native := t.TempDir()
			require.NoError(t, NewWorktreeStore(repo).WriteWorktreeMarker(native, branch, "HSWE-452"))
			if tc.generated {
				require.NoError(t, RecordGeneratedPaths(native, []string{"AGENTS.md"}))
			}
			require.NoError(t, os.WriteFile(filepath.Join(native, "AGENTS.md"), []byte(tc.host), 0600))
			linked, err := OpenLinked(native, repo.MgitDir(), branch, repo.clock)
			require.NoError(t, err)
			defer func() { _ = linked.Close() }()

			err = NewMergeStore(linked).FastForwardDelivered(ctx, branch, tip)
			head, e := linked.Head()
			require.NoError(t, e)
			got, e := os.ReadFile(filepath.Join(native, "AGENTS.md")) //nolint:gosec // test fixture path
			require.NoError(t, e)
			require.Equal(t, tc.host, string(got), "the host doc is never rewritten by land")
			if tc.lands {
				require.NoError(t, err)
				require.Equal(t, tip, head)
				_, e = os.Stat(filepath.Join(native, "new.txt"))
				require.NoError(t, e)
				return
			}
			var conflict *ProjectionConflictError
			require.ErrorAs(t, err, &conflict)
			require.Equal(t, "AGENTS.md", conflict.Path)
			require.Equal(t, base, head)
		})
	}
}
