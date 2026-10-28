package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/stretchr/testify/require"
)

// A retained, undelivered guest commit is not content a host task may start
// from or a host worktree may materialize. Refs: MGIT-323.2, MGIT-323.3

func TestBaseAdmission_NewTaskBaseRefusesUndeliveredGuestCommit(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "wt")

	_, err := newWorktreeSvcWithSync(e, "head-A").Add(ctx,
		model.WorktreeAddOptions{Path: dest, TaskID: "MGIT-9.1", Base: hash})
	require.ErrorContains(t, err, "undelivered")

	_, err = e.bs.GetBranch(ctx, "task/MGIT-9.1")
	require.ErrorIs(t, err, model.ErrBranchNotFound, "a refused base must not create the task branch")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "a refused base must not materialize a host worktree")
}

func TestBaseAdmission_NewTaskBaseAcceptsHostCommitsAndRefs(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head() // an accepted host commit, delivered or never indexed
	require.NoError(t, err)

	for i, base := range []string{head, "HEAD"} {
		task := []string{"MGIT-9.2", "MGIT-9.3"}[i]
		dest := filepath.Join(t.TempDir(), "wt")
		_, err := newWorktreeSvcWithSync(e, "head-A").Add(ctx,
			model.WorktreeAddOptions{Path: dest, TaskID: task, Base: base})
		require.NoError(t, err, "base %q is accepted host content", base)
	}
}

func TestBaseAdmission_CheckoutRefusesBranchAtUndeliveredContent(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "at-refused", HeadCommit: hash}))
	before, err := e.repo.Head()
	require.NoError(t, err)

	svc := NewCheckoutService(e.bs, gitstore.NewWorktreeStore(e.repo), e.idx)
	_, err = svc.Checkout(ctx, "at-refused")
	require.ErrorContains(t, err, "undelivered")

	after, err := e.repo.Head()
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused checkout must leave HEAD where it was")
	_, statErr := os.Stat(filepath.Join(e.repo.Root(), "refused-step.txt"))
	require.True(t, os.IsNotExist(statErr))
}

func TestBaseAdmission_CheckoutKeepsPlainHostBranchesWorking(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head()
	require.NoError(t, err)
	// A host branch whose tip was never indexed (no task ledger row at all).
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "plain-host", HeadCommit: head}))

	svc := NewCheckoutService(e.bs, gitstore.NewWorktreeStore(e.repo), e.idx)
	res, err := svc.Checkout(ctx, "plain-host")
	require.NoError(t, err)
	require.Equal(t, "switched", res.Status)
}

func TestBaseAdmission_CheckoutWithoutALedgerFailsClosed(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head()
	require.NoError(t, err)
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "plain-host", HeadCommit: head}))

	_, err = NewCheckoutService(e.bs, gitstore.NewWorktreeStore(e.repo), nil).Checkout(ctx, "plain-host")
	require.ErrorContains(t, err, "no content ledger")
}

func TestBaseAdmission_ReaddOfATaskAtAnUndeliveredTipIsRefused(t *testing.T) {
	e, hash := pendingContentFixture(t)
	ctx := context.Background()
	// The guest branch from the fixture ends at the retained guest commit.
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "task/MGIT-9.5", HeadCommit: hash}))
	dest := filepath.Join(t.TempDir(), "wt")

	_, err := newWorktreeSvcWithSync(e, "head-A").Add(ctx,
		model.WorktreeAddOptions{Path: dest, TaskID: "MGIT-9.5"})
	require.ErrorContains(t, err, "undelivered")
	_, statErr := os.Stat(dest)
	require.True(t, os.IsNotExist(statErr), "a refused re-add must not materialize a worktree")
}
