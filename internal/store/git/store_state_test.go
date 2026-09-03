package git

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// recordPoisonedTree writes a tree carrying the given paths verbatim — .git
// components included — and points HEAD at it, reproducing what a PRE-FIX
// mgit recorded before MGIT-157 excluded nested stores per component.
//
// It has to be built with plumbing: since the fix, no CLI path can produce
// such a tree, which is exactly why the at-rest check exists. A test that
// tried to poison a repo through the normal verbs would silently assert
// nothing. Refs: MGIT-157, MGIT-162
func recordPoisonedTree(t *testing.T, repo *Repository, paths ...string) {
	t.Helper()
	blob, err := writeBlob(repo.repo.Storer, []byte("content\n"))
	require.NoError(t, err)

	files := map[string]blobEntry{"ordinary.txt": {hash: blob, mode: filemode.Regular}}
	for _, p := range paths {
		files[p] = blobEntry{hash: blob, mode: filemode.Regular}
	}
	treeHash, err := writeNestedTree(repo.repo.Storer, files)
	require.NoError(t, err)

	head, err := repo.currentRef()
	require.NoError(t, err)
	sig := object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0).UTC()}
	c := &object.Commit{Author: sig, Committer: sig, TreeHash: treeHash, Message: "poisoned"}
	obj := repo.repo.Storer.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	commitHash, err := repo.repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, repo.repo.Storer.SetReference(
		plumbing.NewHashReference(head.Name(), commitHash)))
}

// RecordedNestedRepos is what `mgit doctor tree/nested-git` calls, and it had
// no coverage from anywhere in the tree — the check was tested against a fake
// scan function, and the real scan against nothing.
//
// The condition it answers is the MGIT-157 damage AT REST: a tree recorded
// before the exclusion existed breaks every later `mgit work`, and until this
// check there was no way to learn that except by failing.
// Refs: MGIT-162, MGIT-157
func TestRecordedNestedRepos(t *testing.T) {
	tests := []struct {
		name     string
		recorded []string
		want     []string
	}{
		{
			name: "a_clean_tree_reports_nothing",
			want: nil,
		},
		{
			name:     "a_nested_git_is_found_by_its_directory_not_its_files",
			recorded: []string{"testdata/inner/.git/HEAD", "testdata/inner/.git/config"},
			want:     []string{"testdata/inner/.git"},
		},
		{
			name:     "a_store_at_the_root_is_found",
			recorded: []string{".git/HEAD"},
			want:     []string{".git"},
		},
		{
			name:     "an_mgit_store_counts_too",
			recorded: []string{"sub/.mgit/HEAD"},
			want:     []string{"sub/.mgit"},
		},
		{
			name:     "a_deeply_nested_store_is_found",
			recorded: []string{"a/b/c/d/.git/HEAD"},
			want:     []string{"a/b/c/d/.git"},
		},
		{
			name: "several_are_all_reported_so_a_reader_learns_the_extent",
			recorded: []string{
				"vendor/one/.git/HEAD",
				"vendor/two/.git/HEAD",
				"testdata/.git/HEAD",
			},
			want: []string{"vendor/one/.git", "vendor/two/.git", "testdata/.git"},
		},
		{
			name:     "a_path_merely_containing_git_is_not_a_repository",
			recorded: []string{".github/workflows/ci.yml", "a/.gitignore", "src/gitutil.go"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			if len(tt.recorded) > 0 {
				recordPoisonedTree(t, repo, tt.recorded...)
			}

			got, err := NewWorktreeStore(repo).RecordedNestedRepos(context.Background())

			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, got,
				"the scan must name the nested REPOSITORY, not each file inside it — "+
					"a user acts on the directory")
		})
	}
}

// The scanner and the failure diagnosis must never disagree about what counts.
// They share a walk on purpose (MGIT-157), and this pins that they still do:
// whatever the at-rest check reports must also appear in the error a
// materialize produces from the same tree.
//
// Asserted by comparing the two OUTPUTS on one tree rather than by reading
// the source, so an inlined copy of the walk would fail here. Refs: MGIT-162, MGIT-157
func TestRecordedNestedRepos_AgreesWithTheFailureDiagnosis(t *testing.T) {
	repo := initTestRepo(t)
	recordPoisonedTree(t, repo, "vendor/dep/.git/HEAD", "testdata/inner/.git/HEAD")

	found, err := NewWorktreeStore(repo).RecordedNestedRepos(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, found)

	head, err := repo.currentRef()
	require.NoError(t, err)
	mErr := NewWorktreeStore(repo).MaterializeBranchTo(
		context.Background(), head.Name().Short(), filepath.Join(t.TempDir(), "wt"))
	require.Error(t, mErr, "the premise: this tree cannot be materialized")

	for _, path := range found {
		assert.Contains(t, mErr.Error(), path,
			"the check found %s but the failure does not name it: the two have drifted", path)
	}
}

// An unreadable HEAD is an error, not an empty answer. doctor renders an error
// as not-checked WITH a reason and an empty slice as a clean bill of health —
// so returning nil here would turn a broken store into a pass.
// Refs: MGIT-162
func TestRecordedNestedRepos_UnreadableHead_IsAnErrorNotACleanResult(t *testing.T) {
	repo := initTestRepo(t)
	head, err := repo.currentRef()
	require.NoError(t, err)
	// Point the branch at an object that is not there.
	require.NoError(t, repo.repo.Storer.SetReference(
		plumbing.NewHashReference(head.Name(), plumbing.NewHash(strings.Repeat("a", 40)))))

	got, err := NewWorktreeStore(repo).RecordedNestedRepos(context.Background())

	require.Error(t, err, "a store that cannot be read must not report a clean tree")
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "nested-repo scan", "the failure says which scan could not run")
}

// Fingerprint is the cheap "has anything changed" probe the passive snapshot
// cadence rests on (MGIT-110): a fingerprint that did not move means no
// capture is paid for. Both halves matter — a constant one would snapshot
// nothing ever, and an unstable one would snapshot a quiet tree forever.
// Refs: MGIT-110
func TestSnapshotStore_Fingerprint_MovesOnlyWhenTheTreeDoes(t *testing.T) {
	repo := initTestRepo(t)
	ss := NewSnapshotStore(repo)
	writeFile(t, repo, "a.txt", "one")

	first, err := ss.Fingerprint()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	again, err := ss.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, first, again,
		"an unchanged tree must fingerprint identically, or quiescence never arrives")

	tests := []struct {
		name   string
		change func(t *testing.T)
	}{
		{"content_changes", func(t *testing.T) { writeFile(t, repo, "a.txt", "two") }},
		{"a_file_is_added", func(t *testing.T) { writeFile(t, repo, "b.txt", "new") }},
		{"a_file_is_removed", func(t *testing.T) {
			require.NoError(t, os.Remove(filepath.Join(repo.Root(), "b.txt")))
		}},
	}
	prev := first
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.change(t)
			got, ferr := ss.Fingerprint()
			require.NoError(t, ferr)
			assert.NotEqual(t, prev, got, "%s must move the fingerprint", tt.name)
			prev = got
		})
	}
}

// The drift signal round-trips, and its three states are distinguishable:
// never written, written, and damaged. Refs: MGIT-35, ADR-008
func TestSyncState_RoundTripsAndDistinguishesItsThreeStates(t *testing.T) {
	repo := initTestRepo(t)

	got, found, err := repo.ReadSyncState()
	require.NoError(t, err, "a store that has never synced is an ordinary case")
	assert.False(t, found, "never-written must be reported as not found, not as a zero value")
	assert.Zero(t, got)

	want := SyncState{GitHead: "abc123", WorkTreeHash: "def456", BaseCommit: "ghi789", SyncedAt: "2026-08-23T00:00:00Z"}
	require.NoError(t, repo.WriteSyncState(want))

	got, found, err = repo.ReadSyncState()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, got)

	// A DAMAGED signal must be a hard error, never a silent reset: resetting
	// would make the next command believe nothing has drifted and skip a
	// resync that was owed. Refs: MGIT-35
	require.NoError(t, os.WriteFile(repo.syncStatePath(), []byte("{not json"), 0o600))
	_, _, err = repo.ReadSyncState()
	require.Error(t, err, "a corrupt signal must not read as 'never synced'")
	assert.Contains(t, err.Error(), repo.syncStatePath(), "the failure names the file to look at")
}

// The write is atomic, so an interrupted one cannot leave a half-parsed signal
// behind. Asserted through its consequence — no leftover temp file survives a
// successful write, and the state file is always wholly parseable — rather
// than by inspecting the implementation. Refs: MGIT-35, ADR-008
func TestWriteSyncState_LeavesNoPartialFileBehind(t *testing.T) {
	repo := initTestRepo(t)

	for i := range 3 {
		require.NoError(t, repo.WriteSyncState(SyncState{GitHead: strings.Repeat("x", i+1)}))
		_, _, err := repo.ReadSyncState()
		require.NoError(t, err, "every write must leave a parseable file")
	}

	entries, err := os.ReadDir(repo.MgitDir())
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-",
			"a completed write must not leave its temp file in the store")
	}
}

// The apply journal is what makes rollback and cherry-pick survive a crash
// between "the commit object exists" and "the branch ref advanced". Its three
// verbs had no coverage at all.
//
// The clearing case is the one worth stating: clearing an absent journal must
// be a no-op, because recovery runs on every open and the common case is that
// there is nothing to recover. Refs: MGIT-54
func TestApplyJournal_RoundTripsAndClearsIdempotently(t *testing.T) {
	repo := initTestRepo(t)

	_, found, err := repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found, "no pending apply is the common case, not an error")

	require.NoError(t, repo.ClearApplyJournal())
	require.NoError(t, repo.ClearApplyJournal(),
		"clearing an absent journal must stay a no-op; recovery runs on every open")

	want := ApplyJournal{
		Root:        repo.Root(),
		CommitHash:  "aaaa",
		ContentHash: "bbbb",
		Index:       ApplyIndexEntry{TaskID: "MGIT-54", AgentID: "agent-1"},
		Diffs:       []model.FileDiff{{Path: "a.txt", Operation: model.DiffModified, NewHash: "after"}},
	}
	require.NoError(t, repo.WriteApplyJournal(want))

	got, found, err := repo.ReadApplyJournal()
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, want, got, "everything recovery needs must survive the round trip")

	require.NoError(t, repo.ClearApplyJournal())
	_, found, err = repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found)
}

// A journal belongs to the worktree that wrote it. Overwriting another root's
// pending journal would lose THAT worktree's recovery information — the
// commit object exists and the ref has not moved, so without the journal the
// next open sees a tip/disk divergence and the ADR-008 auto-resync silently
// undoes the revert. Refs: MGIT-54
func TestWriteApplyJournal_RefusesToOverwriteAnotherWorktreesPendingApply(t *testing.T) {
	repo := initTestRepo(t)
	other := ApplyJournal{Root: filepath.Join(t.TempDir(), "other-wt"), CommitHash: "cccc"}
	require.NoError(t, repo.WriteApplyJournal(other))

	err := repo.WriteApplyJournal(ApplyJournal{Root: repo.Root(), CommitHash: "dddd"})

	require.Error(t, err, "another worktree's recovery information must not be destroyed")
	assert.Contains(t, err.Error(), other.Root,
		"the refusal must name the directory to run a command from")

	got, found, rerr := repo.ReadApplyJournal()
	require.NoError(t, rerr)
	require.True(t, found)
	assert.Equal(t, other, got, "and the pending journal must be untouched")
}

// Rewriting the SAME root's journal is allowed: a worktree re-entering an
// apply is resuming its own work, not trampling someone else's.
func TestWriteApplyJournal_TheSameRootMayRewriteItsOwn(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.WriteApplyJournal(ApplyJournal{Root: repo.Root(), CommitHash: "first"}))
	require.NoError(t, repo.WriteApplyJournal(ApplyJournal{Root: repo.Root(), CommitHash: "second"}))

	got, found, err := repo.ReadApplyJournal()
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "second", got.CommitHash)
}

// A CORRUPT journal must block the next apply rather than being overwritten.
// It marks an interrupted content-apply whose details can no longer be read;
// proceeding would leave that apply half-done with nothing recording it.
// Refs: MGIT-54
func TestApplyJournal_Corrupt_FailsLoudlyRatherThanBeingOverwritten(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, os.WriteFile(repo.applyJournalPath(), []byte("{truncated"), 0o600))

	_, _, readErr := repo.ReadApplyJournal()
	require.Error(t, readErr, "a damaged journal is not 'no pending apply'")

	writeErr := repo.WriteApplyJournal(ApplyJournal{Root: repo.Root()})
	require.Error(t, writeErr,
		"a write must not silently replace a journal it could not read")

	// It stays clearable, so an operator is not stuck.
	require.NoError(t, repo.ClearApplyJournal())
	_, found, err := repo.ReadApplyJournal()
	require.NoError(t, err)
	assert.False(t, found)
}

// Both pieces of store state live in the SHARED store directory, so a linked
// worktree and its parent read one signal rather than two. A per-worktree copy
// would let them disagree about whether a resync is owed. Refs: MGIT-35, MGIT-54
func TestStoreState_LivesInTheSharedStoreDirectory(t *testing.T) {
	repo := initTestRepo(t)

	for name, path := range map[string]string{
		"sync state":    repo.syncStatePath(),
		"apply journal": repo.applyJournalPath(),
	} {
		assert.Equal(t, repo.MgitDir(), filepath.Dir(path),
			"the %s must sit in the shared store, or worktrees will disagree", name)
	}
}

// The persisted forms are plain JSON. A caller debugging a stuck resync reads
// these by hand, and an opaque encoding would make the one recovery path
// harder than the failure. Refs: MGIT-35, MGIT-54
func TestStoreState_IsReadableJSONOnDisk(t *testing.T) {
	repo := initTestRepo(t)
	require.NoError(t, repo.WriteSyncState(SyncState{GitHead: "abc"}))
	require.NoError(t, repo.WriteApplyJournal(ApplyJournal{Root: repo.Root(), CommitHash: "x"}))

	for _, path := range []string{repo.syncStatePath(), repo.applyJournalPath()} {
		data, err := os.ReadFile(filepath.Clean(path))
		require.NoError(t, err)
		var any map[string]any
		require.NoError(t, json.Unmarshal(data, &any), "%s is not readable JSON", path)
	}
}
