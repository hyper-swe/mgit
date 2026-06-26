package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// materializeSeedFixture commits a tracked source file to HEAD, branches the
// task at HEAD, and returns the branch name + a fresh destination dir. It leaves
// the caller to lay down gitignored working-tree artifacts and an optional
// .mgit/seed-include before calling MaterializeBranchTo. Refs: MGIT-38
func materializeSeedFixture(t *testing.T) (repo *Repository, branch, dest string) {
	t.Helper()
	repo = initTestRepo(t)
	cs := NewCommitStore(repo)
	bs := NewBranchStore(repo)
	ws := NewWorktreeStore(repo)
	ctx := context.Background()
	root := repo.Root()

	// A tracked source file so the branch tree is non-empty.
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "."))
	c := makeTestModelCommit(t, "MGIT-38")
	c.FileDiffs = nil
	head, err := cs.CreateCommit(ctx, c)
	require.NoError(t, err)
	require.NoError(t, bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-38", HeadCommit: head}))
	return repo, "task/MGIT-38", filepath.Join(t.TempDir(), "linked-wt")
}

// TestMaterializeBranchTo_SeedInclude_CarriesGitignoredBuildArtifact proves a
// gitignored-but-build-required path listed in .mgit/seed-include is copied from
// the SOURCE working tree into the materialized worktree, even though it is not
// in the .mgit object tree. Refs: MGIT-38
func TestMaterializeBranchTo_SeedInclude_CarriesGitignoredBuildArtifact(t *testing.T) {
	repo, branch, dest := materializeSeedFixture(t)
	ctx := context.Background()
	root := repo.Root()

	// web/dist is gitignored (never imported into .mgit) but build-required.
	writeFileMk(t, root, ".gitignore", "web/dist/\n")
	writeFileMk(t, root, "web/dist/index.html", "<html></html>\n")
	writeFileMk(t, repo.MgitDir(), "seed-include", "web/dist\n")

	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest))

	got, err := os.ReadFile(filepath.Join(dest, "web", "dist", "index.html")) //nolint:gosec // test path under t.TempDir()
	require.NoError(t, err)
	assert.Equal(t, "<html></html>\n", string(got))
}

// TestMaterializeBranchTo_NoSeedInclude_OmitsGitignoredArtifact proves the
// existing MGIT-32 behavior is preserved: without a seed-include file, the same
// gitignored artifact is NOT carried into the worktree. Refs: MGIT-38, MGIT-32
func TestMaterializeBranchTo_NoSeedInclude_OmitsGitignoredArtifact(t *testing.T) {
	repo, branch, dest := materializeSeedFixture(t)
	ctx := context.Background()
	root := repo.Root()

	writeFileMk(t, root, ".gitignore", "web/dist/\n")
	writeFileMk(t, root, "web/dist/index.html", "<html></html>\n")
	// No .mgit/seed-include written.

	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest))

	_, err := os.Stat(filepath.Join(dest, "web", "dist", "index.html"))
	assert.True(t, os.IsNotExist(err), "without seed-include, a gitignored artifact must not be carried")
}

// TestMaterializeBranchTo_SeedInclude_StillExcludesOtherGitignoredJunk proves
// listing web/dist does not relax exclusion of other gitignored paths
// (node_modules): only the listed globs are carried. Refs: MGIT-38
func TestMaterializeBranchTo_SeedInclude_StillExcludesOtherGitignoredJunk(t *testing.T) {
	repo, branch, dest := materializeSeedFixture(t)
	ctx := context.Background()
	root := repo.Root()

	writeFileMk(t, root, ".gitignore", "web/dist/\nnode_modules/\n")
	writeFileMk(t, root, "web/dist/index.html", "<html></html>\n")
	writeFileMk(t, root, "node_modules/left-pad/index.js", "module.exports = 0\n")
	writeFileMk(t, repo.MgitDir(), "seed-include", "web/dist\n")

	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest))

	_, err := os.Stat(filepath.Join(dest, "web", "dist", "index.html"))
	require.NoError(t, err, "the listed seed-include artifact is carried")
	_, err = os.Stat(filepath.Join(dest, "node_modules", "left-pad", "index.js"))
	assert.True(t, os.IsNotExist(err), "an unlisted gitignored path must still be excluded")
}

// TestMaterializeBranchTo_SeedInclude_AbsentPathSkippedSilently proves a
// seed-include entry with no matching file in the source working tree is skipped
// without error (the path is optional). Refs: MGIT-38
func TestMaterializeBranchTo_SeedInclude_AbsentPathSkippedSilently(t *testing.T) {
	repo, branch, dest := materializeSeedFixture(t)
	ctx := context.Background()

	// seed-include lists a path that does not exist in the source working tree.
	writeFileMk(t, repo.MgitDir(), "seed-include", "web/dist\n# a comment\n\n")

	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest),
		"an absent seed-include path must be skipped silently")

	_, err := os.Stat(filepath.Join(dest, "web", "dist"))
	assert.True(t, os.IsNotExist(err))
}

// TestMaterializeBranchTo_SeedInclude_RejectsTraversalGlob proves a traversal,
// absolute, or .git/.mgit-targeting glob is ignored safely: it copies nothing
// outside destRoot and does not fail the materialize. Refs: MGIT-38
func TestMaterializeBranchTo_SeedInclude_RejectsTraversalGlob(t *testing.T) {
	repo, branch, dest := materializeSeedFixture(t)
	ctx := context.Background()
	root := repo.Root()

	// A real file outside the project root that a traversal glob would target.
	outside := filepath.Join(filepath.Dir(root), "victim.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret\n"), 0o600))
	t.Cleanup(func() { _ = os.Remove(outside) })

	writeFileMk(t, root, ".gitignore", "web/dist/\n")
	writeFileMk(t, root, "web/dist/index.html", "<html></html>\n")
	writeFileMk(t, repo.MgitDir(), "seed-include",
		"../victim.txt\n/etc/passwd\n.git\n.mgit\nweb/../../victim.txt\nweb/dist\n")

	require.NoError(t, NewWorktreeStore(repo).MaterializeBranchTo(ctx, branch, dest),
		"a malicious glob must be ignored, not abort the materialize")

	// Nothing escaped destRoot.
	_, err := os.Stat(filepath.Join(dest, "..", "victim.txt"))
	assert.True(t, os.IsNotExist(err))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, ".git", e.Name(), "a .git glob must never be honored")
		assert.NotEqual(t, ".mgit", e.Name(), "a .mgit glob must never be honored")
	}
	// The one safe entry still carried.
	_, err = os.Stat(filepath.Join(dest, "web", "dist", "index.html"))
	require.NoError(t, err, "the safe seed-include path is still carried")
}
