package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// The guest's private branch shares objects, not a host projection or authority.
func pendingContentFixture(t *testing.T) (*testEnv, string) {
	return pendingContentFixtureOrder(t, false)
}

func pendingContentFixtureOrder(t *testing.T, first bool) (*testEnv, string) {
	t.Helper()
	e := setupTestEnv(t)
	ctx := context.Background()
	base, err := e.repo.Head()
	require.NoError(t, err)
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "guest", HeadCommit: base}))
	root := t.TempDir()
	require.NoError(t, e.wt.WriteWorktreeMarker(root, "guest", "MGIT-323"))
	guest, err := gitstore.OpenLinked(root, e.repo.MgitDir(), "guest", fixedClock())
	require.NoError(t, err)
	t.Cleanup(func() { _ = guest.Close() })
	require.NoError(t, os.WriteFile(filepath.Join(root, "same.txt"), []byte("refused guest overwrite\n"), 0600))
	require.NoError(t, gitstore.NewWorktreeStore(guest).Add(ctx, "same.txt"))
	c := &model.Commit{TaskID: contentTestTaskID(t), AgentID: "test", Message: "[MGIT:MGIT-323] guest"}
	hash, err := gitstore.NewCommitStore(guest).CreateCommit(ctx, c)
	require.NoError(t, err)

	if !first {
		stageAndCommitFiles(t, e, "MGIT-323", "accepted host", map[string]string{"same.txt": "accepted host\n", "accepted.txt": "host only\n"})
	}
	raw, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, e.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: hash, ContentHash: c.ContentHash, Position: len(raw), Origin: index.OriginLand}}))
	if first {
		stageAndCommitFiles(t, e, "MGIT-323", "accepted host", map[string]string{"same.txt": "accepted host\n", "accepted.txt": "host only\n"})
	}

	return e, hash
}

func TestContentEligibility_EmittersExcludePending(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	preview, err := e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	require.Contains(t, preview.Patch, "accepted host")
	require.NotContains(t, preview.Patch, "refused guest overwrite")
	diff := NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx)
	changes, err := diff.DiffTask(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Contains(t, diff.FormatUnified(changes), "accepted host")
	squash, err := e.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	patch, err := e.squash.GitFormatPatch(ctx, squash)
	require.NoError(t, err)
	require.NotContains(t, patch, "refused guest overwrite")
}

func TestContentEligibility_SourceRefusedBeforeMutation(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head()
	require.NoError(t, err)
	rows, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	_, err = e.commit.CherryPick(ctx, CherryPickRequest{SourceHash: hash, TaskID: "MGIT-324"})
	require.ErrorContains(t, err, "undelivered")
	after, err := e.repo.Head()
	require.NoError(t, err)
	require.Equal(t, head, after)
	ledger, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Equal(t, rows, ledger)
}

func TestContentEligibility_AmbiguousLegacyRefusesAll(t *testing.T) {
	for _, op := range []string{"squash", "preview", "diff", "rollback", "pick"} {
		t.Run(op, func(t *testing.T) {
			e := setupTestEnv(t)
			ctx := context.Background()
			stageAndCommitFiles(t, e, "MGIT-1", "base", map[string]string{"base.txt": "base\n"})
			c := &model.Commit{TaskID: contentTestTaskID(t), AgentID: "local", Message: "[MGIT:MGIT-323] legacy"}
			require.NoError(t, os.WriteFile(filepath.Join(e.repo.Root(), "old.txt"), []byte("old\n"), 0600))
			require.NoError(t, e.wt.Add(ctx, "old.txt"))
			hash, err := e.cs.CreateCommit(ctx, c)
			require.NoError(t, err)
			require.NoError(t, e.idx.AppendTaskCommit(ctx, index.TaskCommitInsert{TaskID: "MGIT-323", CommitHash: hash, ContentHash: c.ContentHash}))
			head, err := e.repo.Head()
			require.NoError(t, err)
			switch op {
			case "squash":
				_, err = e.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-323"})
			case "preview":
				_, err = e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "MGIT-323"})
			case "diff":
				_, err = NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx).DiffTask(ctx, "MGIT-323")
			case "rollback":
				_, err = e.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-323"})
			case "pick":
				_, err = e.commit.CherryPick(ctx, CherryPickRequest{SourceHash: hash})
			}
			require.ErrorContains(t, err, "ambiguous content eligibility")
			after, err := e.repo.Head()
			require.NoError(t, err)
			require.Equal(t, head, after)
		})
	}
}

func contentTestTaskID(t *testing.T) model.TaskID {
	t.Helper()
	id, err := model.ParseTaskID("MGIT-323")
	require.NoError(t, err)
	return id
}

func TestContentEligibility_RestoreCannotApplyPending(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "all"}[all], func(t *testing.T) {
			e, hash := pendingContentFixture(t)
			ctx := context.Background()
			restore := NewRestoreService(e.repo, e.cs, e.repo.Root(), e.idx)
			var err error
			if all {
				_, err = restore.RestoreAll(ctx, hash, true)
			} else {
				_, err = restore.RestoreFile(ctx, "same.txt", hash)
			}
			require.ErrorContains(t, err, "undelivered")
			require.Equal(t, "accepted host\n", readWorking(t, e, "same.txt"))
		})
	}
}

func TestContentEligibility_ExplicitRollbackPendingSource(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	// The task also has eligible host work. Selecting its refused source hash
	// must NOT silently roll that accepted work back instead.
	before, err := e.repo.Head()
	require.NoError(t, err)
	_, err = e.rollbk.RollbackTask(ctx, RollbackRequest{TaskID: "MGIT-323", SourceHash: hash})
	require.ErrorContains(t, err, "undelivered")
	after, err := e.repo.Head()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "accepted host\n", readWorking(t, e, "same.txt"))
}

func TestContentEligibility_RefusedFirstKeepsPinnedBase(t *testing.T) {
	e, _ := pendingContentFixtureOrder(t, true)
	ctx := context.Background()
	eligible, err := e.idx.GetContentCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.Len(t, eligible, 1)
	c, err := e.cs.GetCommit(ctx, eligible[0].CommitHash)
	require.NoError(t, err)
	require.NoError(t, e.idx.InsertWorktree(ctx, &model.WorktreeInfo{Path: e.repo.Root(), TaskID: "MGIT-323", Branch: model.TaskBranchName("MGIT-323"), ForkBase: c.ParentID}))
	patch, err := e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	require.Contains(t, patch.Patch, "accepted host")
	require.NotContains(t, patch.Patch, "refused guest overwrite")
}

func TestContentEligibility_DeliveredGuestRepeatSquashAndHostWork(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	// Unit boundary fixture: model the trusted projector's successful admission;
	// cmd/mgit-sandboxd tests exercise actual disk/ref read-back and recovery.
	rows, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	for _, r := range rows {
		if r.CommitHash == hash {
			require.NoError(t, e.idx.RecordDelivered(ctx, []index.CommitRecord{r}))
		}
	}
	first, err := e.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	stageAndCommitFiles(t, e, "MGIT-323", "new host after squash", map[string]string{"same.txt": "new host after squash\n"})
	second, err := e.squash.SquashTask(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	patch, err := e.squash.GitFormatPatch(ctx, second)
	require.NoError(t, err)
	require.Contains(t, patch, "+new host after squash")
	require.Contains(t, patch, "+host only")
	require.NotContains(t, patch, "+refused guest overwrite")
	require.NoError(t, e.idx.RequireContentCommit(ctx, first.CommitID), "squash ref rewriting must not revoke accepted source rows")
	require.NoError(t, e.idx.RequireContentCommit(ctx, hash))
	again, err := e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.NoError(t, err)
	require.Contains(t, again.Patch, "+new host after squash")
}

func TestContentEligibility_MissingEligibleObjectIsLoud(t *testing.T) {
	e := setupTestEnv(t)
	ctx := context.Background()
	require.NoError(t, e.idx.AddCommitToTask(ctx, "MGIT-323", "1111111111111111111111111111111111111111", "content", "host", 0))
	_, err := e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "MGIT-323"})
	require.Error(t, err)
	_, err = NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx).DiffTask(ctx, "MGIT-323")
	require.Error(t, err)
}

func TestContentEligibility_ByHashDiffRefusesPendingEndpoints(t *testing.T) {
	e, pending := pendingContentFixture(t)
	ctx := context.Background()
	rows, err := e.idx.GetContentCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	accepted := rows[0].CommitHash
	svc := NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx)
	for name, diff := range map[string]func(context.Context, string, string) ([]model.FileDiff, error){"commits": svc.DiffCommits, "range": svc.DiffRange} {
		t.Run(name, func(t *testing.T) {
			for _, pair := range [][2]string{{accepted, pending}, {pending, accepted}, {accepted, pending[:12]}} {
				changes, err := diff(ctx, pair[0], pair[1])
				require.ErrorContains(t, err, "undelivered")
				require.Empty(t, changes)
			}
			changes, err := diff(ctx, accepted, accepted)
			require.NoError(t, err)
			require.Empty(t, changes)
		})
	}
}

func TestContentEligibility_AllPendingIsLoud(t *testing.T) {
	e := setupTestEnv(t)
	ctx := context.Background()
	stageAndCommitFiles(t, e, "PENDING-1", "pending only", map[string]string{"pending.txt": "guest only\n"})
	hash, err := e.repo.Head()
	require.NoError(t, err)
	// A distinct task binds the same retained object as not yet delivered.
	require.NoError(t, e.idx.AppendTaskCommit(ctx, index.TaskCommitInsert{TaskID: "PENDING-2", CommitHash: hash, ContentHash: "retained", Origin: index.OriginLand}))
	svc := NewDiffService(gitstore.NewDiffStore(e.repo), e.cs, e.idx)
	changes, err := svc.DiffTask(ctx, "PENDING-2")
	require.Empty(t, changes)
	require.ErrorContains(t, err, "no delivered content")
	require.ErrorContains(t, err, "mgit init")
	require.ErrorContains(t, err, "mgit commit -a --task-id RECOVERY-1")
	_, err = e.squash.PreviewGitPatch(ctx, SquashRequest{TaskID: "PENDING-2"})
	require.ErrorContains(t, err, "no delivered content")
	require.ErrorContains(t, err, "mgit commit -a --task-id RECOVERY-1")
}
