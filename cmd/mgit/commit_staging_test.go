package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// MGIT-77: `mgit commit` with nothing staged used to print a hash and exit 0
// while recording a tree byte-identical to its parent. These tests pin the
// contract at the process boundary an agent actually observes: the exit code,
// the message, and the tree. Refs: FR-2, FR-8.3, MGIT-77

// commitTreeInfo reads a commit's tree hash and its parent's tree hash from the
// .mgit store via the store layer (never by shelling out to git).
func commitTreeInfo(t *testing.T, repoRoot, commitID string) (tree, parentTree string) {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	cs := gitstore.NewCommitStore(r)
	ctx := context.Background()
	c, err := cs.GetCommit(ctx, commitID)
	require.NoError(t, err)
	require.NotEmpty(t, c.ParentID, "commit %s has no parent to compare against", commitID)
	p, err := cs.GetCommit(ctx, c.ParentID)
	require.NoError(t, err)
	return c.TreeHash, p.TreeHash
}

// headCommitID returns the commit id at the repository's current ref.
func headCommitID(t *testing.T, repoRoot string) string {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	head, err := r.Head()
	require.NoError(t, err)
	return head
}

// fileInCommit returns the content of path in the given commit, or "" if absent.
func fileInCommit(t *testing.T, repoRoot, commitID, path string) string {
	t.Helper()
	r, err := gitstore.Open(repoRoot, func() time.Time { return time.Now().UTC() })
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	data, err := gitstore.NewCommitStore(r).GetFileFromCommit(context.Background(), commitID, path)
	if err != nil {
		return ""
	}
	return string(data)
}

// TestCommit_EmptyCommitRefusal_RealProcess pins BOTH halves of acceptance
// criterion 1: the process exit status an agent branches on, and the tree
// condition the exit status is supposed to be reporting on. It runs the
// compiled binary because an in-process cobra error says nothing about what
// `echo $?` would have printed. Refs: MGIT-77
func TestCommit_EmptyCommitRefusal_RealProcess(t *testing.T) {
	bin := buildMgitTestBinary(t)
	repo := t.TempDir()
	require.NoError(t, runMgitTest(t, bin, repo, "init"))

	// Seed one real commit so HEAD has content to compare a later tree against.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600))
	require.NoError(t, runMgitTest(t, bin, repo, "add", "seed.txt"))
	require.NoError(t, runMgitTest(t, bin, repo, "commit", "--task-id", "MGIT-77", "-m", "seed"))
	headBefore := headCommitID(t, repo)

	t.Run("nothing_staged_exits_nonzero_and_names_the_remedy", func(t *testing.T) {
		// The reported shape: real work on disk, never staged.
		require.NoError(t, os.WriteFile(filepath.Join(repo, "work.txt"),
			[]byte("work the agent believes it checkpointed\n"), 0o600))

		out, code := runMgitTestExit(t, bin, repo, "commit", "--task-id", "MGIT-77", "-m", "unstaged")
		assert.NotZero(t, code, "an unrecorded commit must not report success; got exit 0 with: %s", out)
		assert.Contains(t, out, "mgit add", "the message must name the remedy")
		assert.Contains(t, out, "--allow-empty", "the message must name the deliberate-empty escape hatch")
		assert.NotContains(t, out, "Usage:\n  mgit commit",
			"the flag table must not bury the remedy — this is a runtime condition, not misuse")

		// The other half of the criterion: nothing was written. HEAD is where it
		// was, so no commit with a parent-identical tree exists.
		assert.Equal(t, headBefore, headCommitID(t, repo),
			"a refused commit must leave the branch tip untouched")
	})

	t.Run("allow_empty_still_succeeds_with_nothing_staged", func(t *testing.T) {
		out, code := runMgitTestExit(t, bin, repo, "commit", "--task-id", "MGIT-77",
			"-m", "deliberate empty", "--allow-empty")
		require.Zero(t, code, "--allow-empty must keep its documented meaning: %s", out)

		head := headCommitID(t, repo)
		tree, parentTree := commitTreeInfo(t, repo, head)
		assert.Equal(t, parentTree, tree, "the deliberate empty commit's tree equals its parent's")
	})

	t.Run("staged_change_is_recorded_in_the_tree", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, "recorded.txt"),
			[]byte("real content\n"), 0o600))
		require.NoError(t, runMgitTest(t, bin, repo, "add", "recorded.txt"))
		require.NoError(t, runMgitTest(t, bin, repo, "commit", "--task-id", "MGIT-77", "-m", "record"))

		head := headCommitID(t, repo)
		tree, parentTree := commitTreeInfo(t, repo, head)
		assert.NotEqual(t, parentTree, tree,
			"the commit tree must differ from its parent — the assertion whose absence let MGIT-77 ship")
		assert.Equal(t, "real content\n", fileInCommit(t, repo, head, "recorded.txt"))
	})
}

// TestCommit_StageAllFlag_RecordsWorkInOneCommand covers the single-command
// loop the generated agent instructions tell an agent to run. A two-command
// loop is a loop an agent drops half of; `-a` removes the opportunity.
// Refs: MGIT-77
func TestCommit_StageAllFlag_RecordsWorkInOneCommand(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	require.NoError(t, os.WriteFile(filepath.Join(repo, "brand-new.txt"), []byte("new file\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "sub", "nested.txt"), []byte("nested\n"), 0o600))

	// No `mgit add` at all — the flag must stage and commit in one step.
	require.NoError(t, runCLI(t, "commit", "-a", "--task-id", "MGIT-77", "-m", "stage and commit"))

	head := headCommitID(t, repo)
	tree, parentTree := commitTreeInfo(t, repo, head)
	assert.NotEqual(t, parentTree, tree, "`mgit commit -a` must record the work")
	assert.Equal(t, "new file\n", fileInCommit(t, repo, head, "brand-new.txt"),
		"an untracked new file is the common agent case and must be included")
	assert.Equal(t, "nested\n", fileInCommit(t, repo, head, "sub/nested.txt"))
}

func TestCommit_StageAllFlag_NoChanges_ReturnsErrNothingToCommit(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))

	err := runCLI(t, "commit", "-a", "--task-id", "MGIT-77", "-m", "nothing at all")
	require.Error(t, err, "-a must not manufacture a change out of a clean tree")
	assert.Contains(t, err.Error(), "nothing to commit")
}

// TestGeneratedClaudeMd_CommitInstruction_StagesTheWork is acceptance criterion
// 4 as an executable check: take the literal mgit command the generated
// CLAUDE.md tells an agent to run, run exactly that in a fresh repo, and prove
// the resulting commit contains the work. Before MGIT-77 the generated block
// said `mgit commit -m "<what changed>"` with no staging step, so an agent
// following it produced a branch of empty commits. Refs: MGIT-77
func TestGeneratedClaudeMd_CommitInstruction_StagesTheWork(t *testing.T) {
	section := agentadapter.RenderClaudeMdSection(agentadapter.SandboxEnv{
		WorktreePath: "/tmp/example-wt",
		Containment:  agentadapter.ContainmentOpen,
	})

	cmdLine := extractGeneratedCommitCommand(t, section)
	require.NotEmpty(t, cmdLine, "the generated discipline must contain a concrete commit command")

	repo := t.TempDir()
	t.Chdir(repo)
	require.NoError(t, runCLI(t, "init"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "agent-work.txt"), []byte("work\n"), 0o600))

	// Drop the "mgit" argv[0] the doc shows; runCLI drives the root command.
	fields := strings.Fields(cmdLine)[1:]
	args := make([]string, 0, len(fields)+2)
	args = append(args, fields...)
	args = append(args, "--task-id", "MGIT-77")
	require.NoError(t, runCLI(t, args...),
		"the generated instruction %q must succeed as written", cmdLine)

	head := headCommitID(t, repo)
	tree, parentTree := commitTreeInfo(t, repo, head)
	assert.NotEqual(t, parentTree, tree,
		"an agent following the generated instructions literally must produce a commit containing its work")
	assert.Equal(t, "work\n", fileInCommit(t, repo, head, "agent-work.txt"))
}

// commitError must only special-case the nothing-to-commit sentinel; every
// other failure keeps its plain "commit: ..." wrapping. Refs: MGIT-77
func TestCommitError_OnlyTheNothingToCommitSentinelGetsTheRemedy(t *testing.T) {
	remedy := commitError(fmt.Errorf("wrapped: %w", model.ErrNothingToCommit))
	require.ErrorIs(t, remedy, model.ErrNothingToCommit, "the sentinel must stay unwrappable")
	assert.Contains(t, remedy.Error(), "mgit add")

	other := commitError(errors.New("disk on fire"))
	assert.Equal(t, "commit: disk on fire", other.Error())
	assert.NotContains(t, other.Error(), "mgit add")
}

// extractGeneratedCommitCommand pulls the checkpoint command out of the
// generated "mgit working discipline" section: the first backticked
// `mgit commit ... -m ...` invocation an agent is told to run each step. The
// placeholder message is replaced with a concrete one (the shell would have
// quoted it; strings.Fields would otherwise split it).
func extractGeneratedCommitCommand(t *testing.T, section string) string {
	t.Helper()
	start := strings.Index(section, "### mgit working discipline")
	require.Positive(t, start, "generated section must carry the working discipline")

	for rest := section[start:]; ; {
		i := strings.Index(rest, "`mgit commit")
		if i < 0 {
			return ""
		}
		rest = rest[i+1:]
		j := strings.Index(rest, "`")
		require.Positive(t, j, "unterminated backtick in generated section")
		line := rest[:j]
		rest = rest[j:]
		if !strings.Contains(line, "-m ") {
			continue // a bare mention, not the per-step checkpoint command
		}
		return strings.ReplaceAll(line, `"<what changed>"`, "generated-instruction-commit")
	}
}
