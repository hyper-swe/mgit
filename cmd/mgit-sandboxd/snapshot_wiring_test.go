package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// snapRig is a repository with one task-bound worktree, laid out the way the
// daemon finds them.
type snapRig struct {
	repoRoot string
	worktree string
	taskID   string
	clock    func() time.Time
	logs     *bytes.Buffer
	logger   *slog.Logger
}

func newSnapRig(t *testing.T, taskID string) snapRig {
	t.Helper()
	now := time.Unix(0, 0).UTC()
	clock := func() time.Time { now = now.Add(time.Second); return now }

	repoRoot := t.TempDir()
	repo, err := gitstore.Init(repoRoot, clock)
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(), &model.Branch{Name: taskBranch(taskID)}))
	require.NoError(t, repo.Close())

	worktree := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(worktree, 0o750))

	logs := &bytes.Buffer{}
	return snapRig{
		repoRoot: repoRoot, worktree: worktree, taskID: taskID, clock: clock, logs: logs,
		logger: slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func (r snapRig) sandbox() model.SandboxInfo {
	return model.SandboxInfo{ID: "sbx-" + r.taskID, TaskID: r.taskID, WorktreePath: r.worktree}
}

func (r snapRig) write(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(r.worktree, name), []byte(content), 0o600))
}

// snapshotIDs lists the passive snapshots recorded for a task.
func (r snapRig) snapshotIDs(t *testing.T) []string {
	t.Helper()
	repo, err := gitstore.OpenLinked(r.worktree,
		filepath.Join(r.repoRoot, ".mgit"), taskBranch(r.taskID), r.clock)
	require.NoError(t, err)
	defer func() { _ = repo.Close() }()
	snaps, err := gitstore.NewSnapshotStore(repo).List(context.Background(), r.taskID)
	require.NoError(t, err)
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.ID)
	}
	return out
}

func listing(sandboxes ...model.SandboxInfo) func(context.Context) ([]model.SandboxInfo, error) {
	return func(context.Context) ([]model.SandboxInfo, error) { return sandboxes, nil }
}

// THE PROPERTY THE WHOLE FEATURE RESTS ON: quiescence state survives between
// passes.
//
// The watcher opens the worktree per pass and closes it again — by design,
// since the CLI writes to the same store between ticks. The FINGERPRINT
// HISTORY must not be short-lived with it. If it were, every pass would look
// like a first observation, nothing would ever settle, and the watcher would
// run forever capturing nothing: a recovery guarantee that is absent and looks
// identical to a working one.
//
// This is what `perTask` and SnapshotService.Rebind exist for, and neither had
// any coverage. Refs: MGIT-110, MGIT-109, R-H234
func TestSnapshotWatcher_QuiescenceSurvivesBetweenPasses(t *testing.T) {
	rig := newSnapRig(t, "LOST-WORK")
	rig.write(t, "important.go", "// thirty minutes of work the agent never committed")
	w := newSnapshotWatcher(listing(rig.sandbox()), rig.repoRoot, rig.clock, rig.logger)
	ctx := context.Background()

	// Pass 1 establishes a baseline. Nothing has settled yet.
	require.NoError(t, w.Observe(ctx))
	assert.Empty(t, rig.snapshotIDs(t),
		"a first observation records a baseline, not a snapshot: a tree seen once "+
			"has not been shown to have stopped changing")

	// Pass 2 over the SAME bytes: the tree has settled.
	require.NoError(t, w.Observe(ctx))
	assert.Len(t, rig.snapshotIDs(t), 1,
		"a tree unchanged across two passes has settled and must be captured; "+
			"if the fingerprint history did not survive the reopen, this stays empty")
}

// A settled tree is captured ONCE, not on every subsequent tick. Capturing
// every pass would bury the useful states among near-duplicates and make the
// namespace grow with idle time rather than with work. Refs: MGIT-110
func TestSnapshotWatcher_ASettledTreeIsCapturedOnceNotEveryTick(t *testing.T) {
	rig := newSnapRig(t, "IDLE-1")
	rig.write(t, "a.go", "stable")
	w := newSnapshotWatcher(listing(rig.sandbox()), rig.repoRoot, rig.clock, rig.logger)
	ctx := context.Background()

	for range 5 {
		require.NoError(t, w.Observe(ctx))
	}

	assert.Len(t, rig.snapshotIDs(t), 1, "an idle worktree must not accrue a snapshot per tick")
}

// Work that CHANGES is captured again once it settles again — the cadence that
// makes an interrupted run lose minutes rather than everything. Refs: MGIT-110, MGIT-109
func TestSnapshotWatcher_EachSettledStateIsCaptured(t *testing.T) {
	rig := newSnapRig(t, "WORKING-1")
	w := newSnapshotWatcher(listing(rig.sandbox()), rig.repoRoot, rig.clock, rig.logger)
	ctx := context.Background()

	rig.write(t, "a.go", "v1")
	require.NoError(t, w.Observe(ctx)) // baseline
	require.NoError(t, w.Observe(ctx)) // settled -> capture #1
	require.Len(t, rig.snapshotIDs(t), 1)

	rig.write(t, "a.go", "v2")
	require.NoError(t, w.Observe(ctx)) // changed -> no capture
	assert.Len(t, rig.snapshotIDs(t), 1, "a tree caught mid-edit must not be captured")

	require.NoError(t, w.Observe(ctx)) // settled again -> capture #2
	assert.Len(t, rig.snapshotIDs(t), 2, "the new settled state is its own recovery point")
}

// ONE UNREADABLE WORKTREE MUST NOT COST EVERY OTHER TASK ITS RECOVERY POINT.
//
// The pass runs over every supervised task, and the failure mode being guarded
// is a loop that returns on the first error: the tasks after it would silently
// go unobserved, and their agents would lose work for a reason belonging to
// someone else's worktree. Refs: MGIT-110
func TestSnapshotWatcher_OneBadTaskDoesNotStopTheOthers(t *testing.T) {
	good := newSnapRig(t, "GOOD-1")
	good.write(t, "a.go", "content")

	// Same repo root, a worktree path that is not there.
	broken := model.SandboxInfo{ID: "sbx-bad", TaskID: "BROKEN-1",
		WorktreePath: filepath.Join(t.TempDir(), "does-not-exist")}
	// Ordered so the broken one is observed FIRST: a loop that bails on error
	// would then never reach the good task at all.
	w := newSnapshotWatcher(listing(broken, good.sandbox()), good.repoRoot, good.clock, good.logger)
	ctx := context.Background()

	err1 := w.Observe(ctx)
	err2 := w.Observe(ctx)

	require.Error(t, err1, "the broken task must be reported, not swallowed")
	assert.Contains(t, err1.Error(), "BROKEN-1", "the failure must name the task it belongs to")
	require.Error(t, err2)
	assert.Len(t, good.snapshotIDs(t), 1,
		"the healthy task must still have been observed on both passes")
}

// A sandbox with nothing to observe is skipped rather than errored. Registration
// is durable and a sandbox can legitimately carry no worktree binding; treating
// that as a failure would fill the log with noise that hides real ones.
func TestSnapshotWatcher_SandboxesWithNothingToObserve_AreSkippedQuietly(t *testing.T) {
	rig := newSnapRig(t, "REAL-1")
	rig.write(t, "a.go", "x")
	w := newSnapshotWatcher(listing(
		model.SandboxInfo{ID: "a", TaskID: "NO-WORKTREE"},
		model.SandboxInfo{ID: "b", WorktreePath: rig.worktree},
		rig.sandbox(),
	), rig.repoRoot, rig.clock, rig.logger)

	require.NoError(t, w.Observe(context.Background()))
	require.NoError(t, w.Observe(context.Background()))

	assert.Len(t, rig.snapshotIDs(t), 1, "the observable task was still observed")
	assert.NotContains(t, rig.logs.String(), "level=ERROR")
}

// A lister that cannot answer is an error for the whole pass — not an empty
// list. An empty list would read as "nothing to supervise" and quietly retire
// the guarantee, which is the MGIT-154 shape. Refs: MGIT-110, MGIT-154
func TestSnapshotWatcher_AListerThatCannotAnswer_IsAFailureNotAnEmptyPass(t *testing.T) {
	rig := newSnapRig(t, "X-1")
	boom := errors.New("registry unavailable")
	w := newSnapshotWatcher(
		func(context.Context) ([]model.SandboxInfo, error) { return nil, boom },
		rig.repoRoot, rig.clock, rig.logger)

	err := w.Observe(context.Background())

	require.ErrorIs(t, err, boom, "an unanswerable pass must say so, not look idle")
	assert.Contains(t, err.Error(), "list sandboxes")
}

// A capture is announced, with what a reader needs to find it later.
func TestSnapshotWatcher_ACaptureIsLoggedWithItsIdentity(t *testing.T) {
	rig := newSnapRig(t, "LOG-1")
	rig.write(t, "a.go", "x")
	w := newSnapshotWatcher(listing(rig.sandbox()), rig.repoRoot, rig.clock, rig.logger)

	require.NoError(t, w.Observe(context.Background()))
	require.NoError(t, w.Observe(context.Background()))

	logs := rig.logs.String()
	assert.Contains(t, logs, "event=snapshot")
	assert.Contains(t, logs, "task_id=LOG-1")
	assert.Contains(t, logs, rig.snapshotIDs(t)[0], "the log must name the snapshot it took")
}

// Two tasks keep SEPARATE quiescence state. Sharing it would let one task's
// edit suppress another's capture. Refs: MGIT-110
func TestSnapshotWatcher_TasksDoNotShareQuiescenceState(t *testing.T) {
	a := newSnapRig(t, "TASK-A")
	a.write(t, "a.go", "stable")

	// A second worktree on the same repo, bound to its own branch.
	repo, err := gitstore.OpenLinked(a.worktree, filepath.Join(a.repoRoot, ".mgit"),
		taskBranch("TASK-A"), a.clock)
	require.NoError(t, err)
	require.NoError(t, gitstore.NewBranchStore(repo).CreateBranch(context.Background(), &model.Branch{Name: taskBranch("TASK-B")}))
	require.NoError(t, repo.Close())

	bWorktree := filepath.Join(t.TempDir(), "wt-b")
	require.NoError(t, os.MkdirAll(bWorktree, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bWorktree, "b.go"), []byte("v1"), 0o600))
	b := model.SandboxInfo{ID: "sbx-b", TaskID: "TASK-B", WorktreePath: bWorktree}

	w := newSnapshotWatcher(listing(a.sandbox(), b), a.repoRoot, a.clock, a.logger)
	ctx := context.Background()

	require.NoError(t, w.Observe(ctx)) // both baseline
	// B keeps changing; A does not.
	require.NoError(t, os.WriteFile(filepath.Join(bWorktree, "b.go"), []byte("v2"), 0o600))
	require.NoError(t, w.Observe(ctx))

	assert.Len(t, a.snapshotIDs(t), 1,
		"a settled task must be captured even while another task is mid-edit")
}

// taskBranch is the convention every worktree binding depends on. It is
// asserted against the literal spelling rather than against itself, because
// the value is a cross-component contract: the CLI's `worktree add`, the
// squash artifact, and this watcher must all name the same ref.
// Refs: MGIT-110, FR-16
func TestTaskBranch_IsTheDocumentedConvention(t *testing.T) {
	assert.Equal(t, "task/MGIT-110", taskBranch("MGIT-110"))
	assert.Equal(t, "task/", taskBranch(""))
}

// buildSnapshotWatcher decides whether the guarantee exists at all, and the
// failure this table exists for is that an ABSENT guarantee looks identical to
// a working one. So each case asserts BOTH the wiring outcome and that the
// daemon operator was told which they have. Refs: MGIT-110, R-H234
func TestBuildSnapshotWatcher(t *testing.T) {
	tests := []struct {
		name      string
		noService bool
		repoRoot  string
		hostRoot  string
		wantOn    bool
		wantLog   string
	}{
		{
			name:     "an_explicit_repo_root_wires_it_on",
			repoRoot: "/repo",
			wantOn:   true,
			wantLog:  "event=snapshot_enabled",
		},
		{
			name:     "the_repo_root_is_recovered_from_the_conventional_host_root",
			hostRoot: "/repo/.mgit/sandbox",
			wantOn:   true,
			wantLog:  "repo_root=/repo",
		},
		{
			name:      "no_service_means_OFF_and_says_so",
			noService: true,
			repoRoot:  "/repo",
			wantOn:    false,
			wantLog:   "event=snapshot_unavailable",
		},
		{
			name:    "an_underivable_root_means_OFF_and_says_so",
			wantOn:  false,
			wantLog: "event=snapshot_unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			logger := slog.New(slog.NewTextHandler(logs, nil))
			var svc sandboxd.SandboxDispatcher
			if !tt.noService {
				svc = stubDispatcher{}
			}

			got := buildSnapshotWatcher(svc, tt.repoRoot, tt.hostRoot,
				func() time.Time { return time.Unix(0, 0).UTC() }, logger)

			assert.Contains(t, logs.String(), tt.wantLog,
				"an operator must be able to tell from the log whether the guarantee exists")
			if tt.wantOn {
				require.NotNil(t, got)
				return
			}
			// A TRUE nil interface, not a typed nil holding a nil pointer.
			// The daemon gates every pass on `Watcher == nil`; a typed nil
			// passes that check and then panics inside Observe on the first
			// tick — which the daemon's recover would log as a snapshot
			// failure forever, with nothing pointing at the wiring.
			assert.Nil(t, got, "an unwired watcher must be a true nil interface")
			assert.True(t, got == nil, "and must satisfy the daemon's own == nil gate") //nolint:testifylint // the identity check IS the assertion
		})
	}
}

// The OFF warnings must say what an operator loses, not just that something is
// off. "snapshots are unavailable" moves the mystery; naming the consequence
// lets them decide whether to care. Refs: MGIT-110, R-H234
func TestBuildSnapshotWatcher_TheOffWarningNamesTheConsequence(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	got := buildSnapshotWatcher(nil, "", "", func() time.Time { return time.Unix(0, 0).UTC() }, logger)

	require.Nil(t, got)
	out := strings.ToLower(logs.String())
	assert.Contains(t, out, "recoverable only from what it committed itself",
		"the warning must state what the operator loses")
}

// stubDispatcher satisfies SandboxDispatcher for wiring tests.
type stubDispatcher struct{}

func (stubDispatcher) Register(context.Context, model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	return nil, nil
}
func (stubDispatcher) Exec(context.Context, string, model.ExecRequest) (*model.ExecResult, error) {
	return nil, nil
}
func (stubDispatcher) List(context.Context) ([]model.SandboxInfo, error) { return nil, nil }
func (stubDispatcher) Remove(context.Context, string, bool) error        { return nil }
func (stubDispatcher) Status(context.Context, string) (*model.SandboxInfo, error) {
	return nil, nil
}
func (stubDispatcher) SyncWorktree(context.Context, string, model.WorktreeSyncOptions) (*model.WorktreeSyncReport, error) {
	return nil, nil
}
