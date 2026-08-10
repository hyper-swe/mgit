package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin MGIT-80: mgit's OWN generated worktree scaffolding
// (the CLAUDE.md block, .claude/settings.json, AGENTS.md, .envrc, the Cursor
// rule) must never be swept into a task commit by a bulk stage — neither when
// it is a brand-new untracked file nor when it is an EDIT to a file the
// project already tracks — while an explicit `mgit add <path>` still stages it.

// recordGen writes the generated-paths manifest for a repo root, failing the
// test on error.
func recordGen(t *testing.T, root string, rels ...string) {
	t.Helper()
	require.NoError(t, RecordGeneratedPaths(root, rels))
}

// commitAllInTestRepo stages and commits everything currently in the working
// tree so subsequent edits are edits to TRACKED files (the second bug shape).
func commitAllInTestRepo(t *testing.T, repo *Repository) {
	t.Helper()
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	require.NoError(t, ws.Add(ctx, "."))
	_, err := NewCommitStore(repo).CreateCommit(ctx, makeTestModelCommit(t, "MGIT-80"))
	require.NoError(t, err)
	require.NoError(t, repo.clearStaging())
}

// TestWorktreeStore_AddAll_GeneratedScaffolding_NotStaged is the core MGIT-80
// regression: a bulk stage records the agent's work and NOT mgit's generated
// scaffolding, in both shapes — scaffolding that is a new untracked file, and
// scaffolding that is a modification of an already-tracked project file.
// Refs: MGIT-80, FR-16
func TestWorktreeStore_AddAll_GeneratedScaffolding_NotStaged(t *testing.T) {
	tests := []struct {
		name string
		// preexisting is written and COMMITTED before the generated block is
		// applied, so those paths are tracked at HEAD.
		preexisting map[string]string
		// generated is the mgit-owned scaffolding written after the commit.
		generated  map[string]string
		wantStaged []string
	}{
		{
			name:        "fresh_worktree_untracked_scaffolding",
			preexisting: map[string]string{"main.go": "package main\n"},
			generated: map[string]string{
				"CLAUDE.md":             "<!-- BEGIN mgit-sandbox -->\nblock\n",
				".claude/settings.json": "{\"hooks\":{}}\n",
			},
			wantStaged: []string{"feature.go"},
		},
		{
			name: "preexisting_tracked_scaffolding_is_an_edit",
			preexisting: map[string]string{
				"main.go":               "package main\n",
				"CLAUDE.md":             "# Project directives\n",
				".claude/settings.json": "{\"model\":\"opus\"}\n",
			},
			generated: map[string]string{
				"CLAUDE.md":             "# Project directives\n<!-- BEGIN mgit-sandbox -->\nblock\n",
				".claude/settings.json": "{\"model\":\"opus\",\"hooks\":{}}\n",
			},
			wantStaged: []string{"feature.go"},
		},
		{
			name: "mixed_tracked_claudemd_and_untracked_envrc",
			preexisting: map[string]string{
				"main.go":   "package main\n",
				"CLAUDE.md": "# Project directives\n",
			},
			generated: map[string]string{
				"CLAUDE.md":                      "# Project directives\n<!-- BEGIN mgit-sandbox -->\n",
				".envrc":                         "export PATH=shims:$PATH\n",
				"AGENTS.md":                      "<!-- BEGIN mgit-sandbox -->\n",
				".cursor/rules/mgit-sandbox.mdc": "rule\n",
			},
			wantStaged: []string{"feature.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			ctx := context.Background()
			ws := NewWorktreeStore(repo)

			for rel, content := range tt.preexisting {
				writeFileMk(t, repo.Root(), rel, content)
			}
			commitAllInTestRepo(t, repo)

			gen := make([]string, 0, len(tt.generated))
			for rel, content := range tt.generated {
				writeFileMk(t, repo.Root(), rel, content)
				gen = append(gen, rel)
			}
			recordGen(t, repo.Root(), gen...)

			// The agent's actual work.
			writeFileMk(t, repo.Root(), "feature.go", "package feature\n")

			require.NoError(t, ws.Add(ctx, "."))
			staged, err := repo.stagedPaths()
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantStaged, staged,
				"a bulk stage must record the agent's work and never mgit's generated scaffolding")
		})
	}
}

// TestWorktreeStore_AddAll_GeneratedFileDeleted_DeletionNotStaged proves the
// exclusion is about provenance, not about the change kind: deleting a
// generated path must not stage a deletion of the project's file either.
// Refs: MGIT-80
func TestWorktreeStore_AddAll_GeneratedFileDeleted_DeletionNotStaged(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "main.go", "package main\n")
	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n")
	commitAllInTestRepo(t, repo)

	recordGen(t, repo.Root(), "CLAUDE.md")
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "CLAUDE.md")))
	writeFileMk(t, repo.Root(), "feature.go", "package feature\n")

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"feature.go"}, staged,
		"a deletion of a generated path is mgit's business, not the task's")
}

// TestWorktreeStore_Add_ExplicitGeneratedPath_StillStages pins the decided
// escape hatch: naming the path explicitly is an unambiguous statement of
// intent, so a user who deliberately wants to commit their own CLAUDE.md edit
// can — only bulk staging is filtered. Refs: MGIT-80
func TestWorktreeStore_Add_ExplicitGeneratedPath_StillStages(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n")
	commitAllInTestRepo(t, repo)
	recordGen(t, repo.Root(), "CLAUDE.md")
	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n\nMy own new rule.\n")

	require.NoError(t, ws.Add(ctx, "CLAUDE.md"))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"CLAUDE.md"}, staged,
		"an explicit pathspec overrides the generated-scaffolding exclusion")
}

// TestWorktreeStore_AddAll_AfterExplicitStage_KeepsGeneratedPathStaged proves
// the escape hatch survives a later bulk stage: `mgit add CLAUDE.md` followed
// by `mgit commit -a` still records the deliberate edit. Refs: MGIT-80
func TestWorktreeStore_AddAll_AfterExplicitStage_KeepsGeneratedPathStaged(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n")
	commitAllInTestRepo(t, repo)
	recordGen(t, repo.Root(), "CLAUDE.md")
	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n\nMy own new rule.\n")
	writeFileMk(t, repo.Root(), "feature.go", "package feature\n")

	require.NoError(t, ws.Add(ctx, "CLAUDE.md"))
	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"CLAUDE.md", "feature.go"}, staged,
		"a bulk stage must not RETRACT a deliberately staged generated path")
}

// TestWorktreeStore_AddAll_NoManifest_StagesEverything proves the exclusion is
// opt-in per worktree: a plain repository with no manifest behaves exactly as
// before. Refs: MGIT-80, MGIT-32
func TestWorktreeStore_AddAll_NoManifest_StagesEverything(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)

	writeFileMk(t, repo.Root(), "CLAUDE.md", "# Project directives\n")
	writeFileMk(t, repo.Root(), "feature.go", "package feature\n")

	require.NoError(t, ws.Add(ctx, "."))
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"CLAUDE.md", "feature.go"}, staged,
		"without a manifest nothing is excluded — this is not a filename blocklist")
}

// TestRecordGeneratedPaths_RepeatedCalls_MergeWithoutDuplicates proves the
// manifest is an idempotent, sorted union so re-running `mgit work` or a
// sandbox relaunch cannot grow it without bound. Refs: MGIT-80
func TestRecordGeneratedPaths_RepeatedCalls_MergeWithoutDuplicates(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, RecordGeneratedPaths(root, []string{"CLAUDE.md", ".envrc"}))
	require.NoError(t, RecordGeneratedPaths(root, []string{"CLAUDE.md", ".claude/settings.json"}))

	got, err := ReadGeneratedPaths(root)
	require.NoError(t, err)
	assert.Equal(t, []string{".claude/settings.json", ".envrc", "CLAUDE.md"}, got,
		"the manifest is the sorted union of every recording")
}

// TestRecordGeneratedPaths_UnsafePath_Rejected proves a path that escapes the
// worktree or names an excluded root is refused rather than silently written:
// the manifest is mgit-authored, so an unsafe entry is a bug, not user input.
// Refs: MGIT-80, MGIT-14.7
func TestRecordGeneratedPaths_UnsafePath_Rejected(t *testing.T) {
	tests := []struct{ name, rel string }{
		{"parent_escape", "../outside.md"},
		{"absolute", "/etc/passwd"},
		{"mgit_store", ".mgit/staging.json"},
		{"project_git", ".git/config"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			err := RecordGeneratedPaths(root, []string{tt.rel})
			require.Error(t, err, "an unsafe generated path must be refused")
			_, statErr := os.Stat(filepath.Join(root, mgitDirName, generatedFileName))
			assert.True(t, os.IsNotExist(statErr), "a refused recording writes no manifest")
		})
	}
}

// TestReadGeneratedPaths_CommentsBlanksAndUnsafeEntries_Ignored proves reading
// is defensive: a hand-edited or corrupt manifest degrades to "exclude less",
// never to an error that would block committing. Refs: MGIT-80
func TestReadGeneratedPaths_CommentsBlanksAndUnsafeEntries_Ignored(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, root, mgitDirName+"/"+generatedFileName,
		"# mgit-generated files\n\nCLAUDE.md\n  .envrc  \n../escape.md\n.mgit/x\n")

	got, err := ReadGeneratedPaths(root)
	require.NoError(t, err)
	assert.Equal(t, []string{"CLAUDE.md", ".envrc"}, got,
		"comments, blanks and unsafe entries are skipped; the rest still applies")
}

// TestReadGeneratedPaths_NoFile_EmptyNoError proves the absent-manifest case is
// a clean zero value, not an error. Refs: MGIT-80
func TestReadGeneratedPaths_NoFile_EmptyNoError(t *testing.T) {
	got, err := ReadGeneratedPaths(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestGeneratedManifest_UnreadableStore_FailsLoud proves an unreadable
// manifest is reported, never silently treated as "nothing is generated" —
// that failure mode is exactly the bug (mgit's scaffolding gets staged), so it
// must not be swallowed. Refs: MGIT-80
func TestGeneratedManifest_UnreadableStore_FailsLoud(t *testing.T) {
	// A regular file where .mgit/ must be a directory makes every manifest
	// operation fail with a non-IsNotExist error.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, mgitDirName), []byte("x\n"), 0o600))

	_, readErr := ReadGeneratedPaths(root)
	require.Error(t, readErr, "an unreadable manifest is an error, not an empty list")
	require.Error(t, RecordGeneratedPaths(root, []string{"CLAUDE.md"}),
		"recording into an unusable store must fail loud")
}

// TestWorktreeStore_AddAll_UnreadableManifest_ReturnsError proves the bulk
// stage refuses rather than proceeding without the exclusion it depends on.
// Refs: MGIT-80
func TestWorktreeStore_AddAll_UnreadableManifest_ReturnsError(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	writeFileMk(t, repo.Root(), "feature.go", "package feature\n")

	// Replace the manifest with a directory so opening it fails.
	require.NoError(t, os.MkdirAll(filepath.Join(repo.MgitDir(), generatedFileName), 0o750))

	err := ws.Add(context.Background(), ".")
	require.Error(t, err, "a bulk stage must not proceed blind to the exclusion list")
	assert.Contains(t, err.Error(), "generated manifest")

	_, err = ws.IsGenerated("CLAUDE.md")
	assert.Error(t, err, "classification propagates the same failure")
}

// TestWorktreeStore_IsGenerated_ClassifiesPathsForTheCLIWarning proves the
// query the CLI uses to tell a user that an explicitly staged path is one mgit
// generated — so the escape hatch is available but never silent. Refs: MGIT-80
func TestWorktreeStore_IsGenerated_ClassifiesPathsForTheCLIWarning(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	recordGen(t, repo.Root(), "CLAUDE.md", ".claude/settings.json")

	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{"generated_root_file", "CLAUDE.md", true},
		{"generated_nested_file", ".claude/settings.json", true},
		{"generated_unnormalized", "./CLAUDE.md", true},
		{"user_file", "feature.go", false},
		{"undeclared_agent_file", "AGENTS.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ws.IsGenerated(tt.rel)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
