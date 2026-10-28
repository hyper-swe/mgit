package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
	"github.com/stretchr/testify/require"
)

// The base, checkout and re-add gates look at a commit's history, not only its
// own ledger row. Refs: MGIT-323.5

// chainReader serves a parent chain from a map and counts the reads.
type chainReader struct {
	parent map[string]string
	reads  int
}

func (c *chainReader) GetCommit(_ context.Context, hash string) (*model.Commit, error) {
	c.reads++
	p, ok := c.parent[hash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}
	return &model.Commit{ParentID: p}, nil
}

func addLandRow(t *testing.T, e *testEnv, hash string, delivered bool) {
	t.Helper()
	ctx := context.Background()
	raw, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, e.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: hash, ContentHash: "c-" + hash, Position: len(raw), Origin: index.OriginLand}}))
	if !delivered {
		return
	}
	raw, err = e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, e.idx.RecordDelivered(ctx, []index.CommitRecord{raw[len(raw)-1]}))
}

func TestDeliverableHistory_HostChildOfUndeliveredCommitIsRefused(t *testing.T) {
	e, guest := pendingContentFixture(t)
	r := &chainReader{parent: map[string]string{"hostchild": guest, guest: ""}}

	err := requireDeliverableHistory(context.Background(), e.idx, r, "hostchild")
	require.ErrorIs(t, err, index.ErrUndeliveredTip)
	require.ErrorContains(t, err, guest, "the message names the offending ancestor")
	require.ErrorContains(t, err, "built on")
}

func TestDeliverableHistory_TipItselfStillRefused(t *testing.T) {
	e, guest := pendingContentFixture(t)
	err := requireDeliverableHistory(context.Background(), e.idx, &chainReader{parent: map[string]string{guest: ""}}, guest)
	require.ErrorIs(t, err, index.ErrUndeliveredTip)
}

func TestDeliverableHistory_StopsAtADeliveredLandRow(t *testing.T) {
	e, guest := pendingContentFixture(t)
	addLandRow(t, e, "mid", true)
	r := &chainReader{parent: map[string]string{"tip": "mid", "mid": guest, guest: ""}}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"),
		"history below a delivered land commit was admitted with it")
}

func TestDeliverableHistory_HostOriginRowStopsNothing(t *testing.T) {
	e, guest := pendingContentFixture(t)
	ctx := context.Background()
	raw, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, e.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: "hostmid", ContentHash: "c-hostmid", Position: len(raw), Origin: index.OriginHost}}))
	r := &chainReader{parent: map[string]string{"tip": "hostmid", "hostmid": guest, guest: ""}}

	err = requireDeliverableHistory(ctx, e.idx, r, "tip")
	require.ErrorIs(t, err, index.ErrUndeliveredTip, "a host row does not vouch for the guest bytes beneath it")
}

func TestDeliverableHistory_ChainBeyondTheBoundRefuses(t *testing.T) {
	e, _ := pendingContentFixture(t)
	parent := map[string]string{}
	for i := 0; i <= maxHistoryWalk+5; i++ {
		parent[fmt.Sprintf("c%d", i)] = fmt.Sprintf("c%d", i+1)
	}
	err := requireDeliverableHistory(context.Background(), e.idx, &chainReader{parent: parent}, "c0")
	require.ErrorIs(t, err, index.ErrUndeliveredTip)
	require.ErrorContains(t, err, "refused rather than passed unchecked")
}

func TestDeliverableHistory_NoUndeliveredCommitReadsNothing(t *testing.T) {
	e := setupTestEnv(t)
	r := &chainReader{parent: map[string]string{"tip": ""}}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"))
	require.Zero(t, r.reads, "an empty undelivered set must not walk")
}

func TestDeliverableHistory_UnreadableCommitFailsClosed(t *testing.T) {
	e, _ := pendingContentFixture(t)
	r := errReader{}
	require.Error(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"))
}

type errReader struct{}

func (errReader) GetCommit(context.Context, string) (*model.Commit, error) {
	return nil, fmt.Errorf("disk gone")
}
