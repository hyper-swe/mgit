package service

import (
	"context"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// A merge integrates the source branch's bytes, so the source tip goes through
// the same history gate as a base or a checkout target. Refs: MGIT-323.4

func TestMergeAdmission_EveryStrategyRefusesAnUndeliveredSource(t *testing.T) {
	for _, strategy := range []MergeStrategy{MergeAuto, MergeNoFF, MergeSquash} {
		t.Run(string(strategy), func(t *testing.T) {
			e, hash := pendingContentFixture(t)
			ctx := context.Background()
			require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "retained", HeadCommit: hash}))
			before, err := e.repo.Head()
			require.NoError(t, err)

			svc := NewMergeService(e.repo, e.bs, gitstore.NewMergeStore(e.repo), e.cs).WithLedger(e.idx)
			_, err = svc.Merge(ctx, MergeRequest{SourceBranch: "retained", Strategy: strategy})
			require.ErrorIs(t, err, index.ErrUndeliveredTip)
			require.ErrorContains(t, err, "retained")

			after, err := e.repo.Head()
			require.NoError(t, err)
			require.Equal(t, before, after, "a refused merge must leave HEAD where it was")
		})
	}
}

func TestMergeAdmission_AHostBranchStillMerges(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head()
	require.NoError(t, err)
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "plain-host", HeadCommit: head}))

	svc := NewMergeService(e.repo, e.bs, gitstore.NewMergeStore(e.repo), e.cs).WithLedger(e.idx)
	res, err := svc.Merge(ctx, MergeRequest{SourceBranch: "plain-host"})
	require.NoError(t, err)
	// plain-host sits at HEAD, so there is nothing to integrate (MGIT-326);
	// the point here is that the admission gate does not refuse it.
	require.Equal(t, "already up to date", res.Status)
}

func TestMergeAdmission_WithoutALedgerFailsClosed(t *testing.T) {
	e, _ := pendingContentFixture(t)
	ctx := context.Background()
	head, err := e.repo.Head()
	require.NoError(t, err)
	require.NoError(t, e.bs.CreateBranch(ctx, &model.Branch{Name: "plain-host", HeadCommit: head}))

	_, err = NewMergeService(e.repo, e.bs, gitstore.NewMergeStore(e.repo), e.cs).Merge(ctx, MergeRequest{SourceBranch: "plain-host"})
	require.ErrorContains(t, err, "no content ledger")
}
