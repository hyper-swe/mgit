package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// BuildSquashTree / PatchFromCommitToTree are the read-only half of the squash
// primitive: they name and diff the tree a squash WOULD produce, so
// `mgit export --format git` can render a real patch without creating a commit.
// MGIT-112 shipped because the export had no such path and rendered from
// dry-run state that was always empty. Refs: MGIT-112, MGIT-22, FR-7

// TestBuildSquashTree_MatchesCreateSquashCommit_WithoutCreatingOne is the
// guarantee the export rests on: the preview names exactly the tree the real
// squash commits, while creating no commit and moving no ref. Refs: MGIT-112
func TestBuildSquashTree_MatchesCreateSquashCommit_WithoutCreatingOne(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	stageCommitFile(t, repo, cs, "BASE-0", "base.go", "package base\n")
	hA := stageCommitFile(t, repo, cs, "MGIT-112", "a.go", "package a\n")
	hB := stageCommitFile(t, repo, cs, "MGIT-112", "b.go", "package b\n")

	headBefore, err := repo.Head()
	require.NoError(t, err)

	preview, err := cs.BuildSquashTree(ctx, []string{hA, hB})
	require.NoError(t, err)
	assert.NotEmpty(t, preview.Tree)
	assert.NotEmpty(t, preview.BaseTree)
	assert.NotEmpty(t, preview.BaseCommit)
	assert.False(t, preview.EmptyNet(), "the task adds two files")

	// No commit created, no ref moved.
	headAfter, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, headBefore, headAfter, "a preview must not advance HEAD")
	_, err = repo.repo.Storer.Reference(plumbing.NewBranchReferenceName("task/MGIT-112"))
	assert.Error(t, err, "a preview must not create the task branch")

	// The real squash lands on exactly the previewed tree.
	squash := makeTestModelCommit(t, "MGIT-112")
	squash.FileDiffs = nil
	squash.CommitType = model.CommitTypeSquash
	_, err = cs.CreateSquashCommit(ctx, SquashCommitParams{
		Commit: squash, TaskCommits: []string{hA, hB}, Branch: "task/MGIT-112",
	})
	require.NoError(t, err)
	assert.Equal(t, preview.Tree, squash.TreeHash,
		"the previewed tree must be the tree the real squash commits")
	assert.Equal(t, preview.BaseCommit, squash.ParentID,
		"the previewed base must be the base the real squash parents off")
}

// TestBuildSquashTree_EmptyNet covers both shapes of a genuinely empty net
// change — a file added then deleted, and a file whose content is restored to
// the base — plus the non-empty control. Refs: MGIT-112
func TestBuildSquashTree_EmptyNet(t *testing.T) {
	tests := []struct {
		name      string
		wantEmpty bool
		// task applies these (path, content) steps as successive commits; a ""
		// content deletes the path.
		steps [][2]string
	}{
		{
			name:      "added_then_deleted_is_empty",
			wantEmpty: true,
			steps:     [][2]string{{"scratch.go", "package scratch\n"}, {"scratch.go", ""}},
		},
		{
			name:      "content_restored_to_base_is_empty",
			wantEmpty: true,
			steps:     [][2]string{{"base.go", "package experiment\n"}, {"base.go", "package base\n"}},
		},
		{
			name:      "real_change_is_not_empty",
			wantEmpty: false,
			steps:     [][2]string{{"base.go", "package changed\n"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			cs := NewCommitStore(repo)
			stageCommitFile(t, repo, cs, "BASE-0", "base.go", "package base\n")

			hashes := make([]string, 0, len(tt.steps))
			for _, step := range tt.steps {
				hashes = append(hashes, applyTaskStep(t, repo, cs, "MGIT-112", step[0], step[1]))
			}

			preview, err := cs.BuildSquashTree(context.Background(), hashes)
			require.NoError(t, err)
			assert.Equal(t, tt.wantEmpty, preview.EmptyNet())
			if tt.wantEmpty {
				assert.Equal(t, preview.BaseTree, preview.Tree,
					"an empty net change leaves the base tree untouched")
			}
		})
	}
}

// applyTaskStep writes or deletes a path, stages it and commits it for the
// task, returning the commit hash.
func applyTaskStep(t *testing.T, repo *Repository, cs *CommitStore, task, path, content string) string {
	t.Helper()
	if content != "" {
		return stageCommitFile(t, repo, cs, task, path, content)
	}
	ctx := context.Background()
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), path)))
	require.NoError(t, NewWorktreeStore(repo).Add(ctx, path))
	c := makeTestModelCommit(t, task)
	c.FileDiffs = nil
	hash, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	return hash
}

// TestBuildSquashTree_RootBase_UsesEmptyTreeAsBase covers a task that began at
// a root commit: there is no base commit, so the base is git's empty tree and
// every file reads as an addition. Refs: MGIT-112, MGIT-22
func TestBuildSquashTree_RootBase_UsesEmptyTreeAsBase(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	head, err := repo.Head()
	require.NoError(t, err)
	root, err := repo.repo.CommitObject(plumbing.NewHash(head))
	require.NoError(t, err)
	require.Zero(t, root.NumParents(), "precondition: the repo's initial commit is a root")

	preview, err := cs.BuildSquashTree(ctx, []string{head})
	require.NoError(t, err)
	assert.Empty(t, preview.BaseCommit, "a root-based task has no base commit")
	assert.Equal(t, emptyTreeHash().String(), preview.BaseTree,
		"its base is git's empty tree, named concretely so EmptyNet is a plain comparison")
}

// TestBuildSquashTree_NoCommits_ReturnsTaskNotFound keeps the read path failing
// loudly rather than yielding a tree that would render an empty patch.
// Refs: MGIT-112
func TestBuildSquashTree_NoCommits_ReturnsTaskNotFound(t *testing.T) {
	cs := NewCommitStore(initTestRepo(t))
	_, err := cs.BuildSquashTree(context.Background(), nil)
	assert.ErrorIs(t, err, model.ErrTaskNotFound)
}

// TestSquashTreePreview_EmptyNet_ZeroValueIsNotEmpty guards the emptiness
// oracle against its most dangerous false positive: a zero-value preview (two
// empty strings) must NOT read as "no net change", or a failure to compute
// would be reported to the user as legitimately empty. Refs: MGIT-112
func TestSquashTreePreview_EmptyNet_ZeroValueIsNotEmpty(t *testing.T) {
	assert.False(t, SquashTreePreview{}.EmptyNet(),
		"an uncomputed preview must never masquerade as an empty net change")
	assert.False(t, SquashTreePreview{Tree: "abc"}.EmptyNet())
	assert.True(t, SquashTreePreview{Tree: "abc", BaseTree: "abc"}.EmptyNet())
}

// TestPatchFromCommitToTree_RendersRealHunks proves the export's renderer emits
// git-apply-correct content for a tree that no commit points at. Refs: MGIT-112
func TestPatchFromCommitToTree_RendersRealHunks(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	stageCommitFile(t, repo, cs, "BASE-0", "base.go", "package base\n")
	h := stageCommitFile(t, repo, cs, "MGIT-112", "added.go", "package added\n")

	preview, err := cs.BuildSquashTree(ctx, []string{h})
	require.NoError(t, err)

	patch, err := NewDiffStore(repo).PatchFromCommitToTree(ctx, preview.BaseCommit, preview.Tree)
	require.NoError(t, err)
	assert.Contains(t, patch, "diff --git a/added.go b/added.go")
	assert.Contains(t, patch, "--- /dev/null", "an added file is git-apply-correct")
	assert.Contains(t, patch, "+package added")
	assert.NotContains(t, patch, "base.go", "an untouched base file must not appear")
}

// TestPatchFromCommitToTree_EmptyBase_TreatsBaseAsEmptyTree covers the
// root-based task: with no base commit every file is an addition. Refs: MGIT-112
func TestPatchFromCommitToTree_EmptyBase_TreatsBaseAsEmptyTree(t *testing.T) {
	repo := initTestRepo(t)
	cs := NewCommitStore(repo)
	ctx := context.Background()

	h := stageCommitFile(t, repo, cs, "MGIT-112", "only.go", "package only\n")
	preview, err := cs.BuildSquashTree(ctx, []string{h})
	require.NoError(t, err)

	patch, err := NewDiffStore(repo).PatchFromCommitToTree(ctx, "", preview.Tree)
	require.NoError(t, err)
	assert.Contains(t, patch, "diff --git a/only.go b/only.go")
	assert.Contains(t, patch, "+package only")
}

// TestPatchRenderers_MissingOperand_FailLoudly is the anti-MGIT-112 guard at the
// store layer: an operand the renderer cannot resolve must be an ERROR, never a
// hunk-free patch that `git apply --allow-empty` accepts and applies to nothing.
// Refs: MGIT-112, MGIT-77
func TestPatchRenderers_MissingOperand_FailLoudly(t *testing.T) {
	repo := initTestRepo(t)
	ds := NewDiffStore(repo)
	ctx := context.Background()
	head, err := repo.Head()
	require.NoError(t, err)
	const absent = "0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name string
		call func() (string, error)
	}{
		{"patch_between_empty_to", func() (string, error) { return ds.PatchBetween(ctx, head, "") }},
		{"patch_between_absent_to", func() (string, error) { return ds.PatchBetween(ctx, head, absent) }},
		{"patch_between_absent_from", func() (string, error) { return ds.PatchBetween(ctx, absent, head) }},
		{"to_tree_empty_tree_hash", func() (string, error) { return ds.PatchFromCommitToTree(ctx, head, "") }},
		{"to_tree_absent_tree", func() (string, error) { return ds.PatchFromCommitToTree(ctx, head, absent) }},
		{"to_tree_absent_from_commit", func() (string, error) {
			tree, terr := repo.repo.CommitObject(plumbing.NewHash(head))
			require.NoError(t, terr)
			return ds.PatchFromCommitToTree(ctx, absent, tree.TreeHash.String())
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, err := tt.call()
			require.Error(t, err, "an unresolvable operand must fail, not render an empty patch")
			assert.Empty(t, strings.TrimSpace(patch))
		})
	}
}
