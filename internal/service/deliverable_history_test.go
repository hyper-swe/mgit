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

// chainReader serves a parent graph from a map and counts the reads. A hash
// absent from the map is a missing object; a root maps to no parents.
type chainReader struct {
	parents map[string][]string
	reads   int
}

func (c *chainReader) Parents(_ context.Context, hash string) ([]string, error) {
	c.reads++
	p, ok := c.parents[hash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}
	return p, nil
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
	r := &chainReader{parents: map[string][]string{"hostchild": {guest}, guest: {}}}

	err := requireDeliverableHistory(context.Background(), e.idx, r, "hostchild")
	require.ErrorIs(t, err, index.ErrUndeliveredTip)
	require.ErrorContains(t, err, guest, "the message names the offending ancestor")
	require.ErrorContains(t, err, "built on")
}

func TestDeliverableHistory_TipItselfStillRefused(t *testing.T) {
	e, guest := pendingContentFixture(t)
	err := requireDeliverableHistory(context.Background(), e.idx, &chainReader{parents: map[string][]string{guest: {}}}, guest)
	require.ErrorIs(t, err, index.ErrUndeliveredTip)
}

func TestDeliverableHistory_StopsAtADeliveredLandRow(t *testing.T) {
	e, guest := pendingContentFixture(t)
	addLandRow(t, e, "mid", true)
	r := &chainReader{parents: map[string][]string{"tip": {"mid"}, "mid": {guest}, guest: {}}}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"),
		"history below a delivered land commit was admitted with it")
}

func TestDeliverableHistory_HostOriginRowStopsNothing(t *testing.T) {
	e, guest := pendingContentFixture(t)
	ctx := context.Background()
	raw, err := e.idx.GetTaskCommits(ctx, "MGIT-323")
	require.NoError(t, err)
	require.NoError(t, e.idx.AppendTaskCommits(ctx, []index.TaskCommitInsert{{TaskID: "MGIT-323", CommitHash: "hostmid", ContentHash: "c-hostmid", Position: len(raw), Origin: index.OriginHost}}))
	r := &chainReader{parents: map[string][]string{"tip": {"hostmid"}, "hostmid": {guest}, guest: {}}}

	err = requireDeliverableHistory(ctx, e.idx, r, "tip")
	require.ErrorIs(t, err, index.ErrUndeliveredTip, "a host row does not vouch for the guest bytes beneath it")
}

func TestDeliverableHistory_FollowsTheSecondParentOfAMerge(t *testing.T) {
	e, guest := pendingContentFixture(t)
	r := &chainReader{parents: map[string][]string{"merge": {"main1", guest}, "main1": {}, guest: {}}}

	err := requireDeliverableHistory(context.Background(), e.idx, r, "merge")
	require.ErrorIs(t, err, index.ErrUndeliveredTip, "a branch merged in is part of the history")
	require.ErrorContains(t, err, guest)
}

func TestDeliverableHistory_ADiamondIsReadOnce(t *testing.T) {
	e, _ := pendingContentFixture(t)
	r := &chainReader{parents: map[string][]string{"tip": {"l", "r"}, "l": {"base"}, "r": {"base"}, "base": {}}}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"))
	require.Equal(t, 4, r.reads, "each commit is read once")
}

func TestDeliverableHistory_LongHostHistoryIsNotRefused(t *testing.T) {
	e, _ := pendingContentFixture(t) // one undelivered row, on an unrelated commit
	parents := map[string][]string{}
	for i := 0; i < 3000; i++ {
		parents[fmt.Sprintf("c%d", i)] = []string{fmt.Sprintf("c%d", i+1)}
	}
	parents["c3000"] = []string{}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, &chainReader{parents: parents}, "c0"),
		"ordinary host work keeps working however long its history")
}

func TestDeliverableHistory_NoUndeliveredCommitReadsNothing(t *testing.T) {
	e := setupTestEnv(t)
	r := &chainReader{parents: map[string][]string{"tip": {}}}

	require.NoError(t, requireDeliverableHistory(context.Background(), e.idx, r, "tip"))
	require.Zero(t, r.reads, "an empty undelivered set must not walk")
}

func TestDeliverableHistory_MissingParentObjectRefuses(t *testing.T) {
	e, _ := pendingContentFixture(t)
	r := &chainReader{parents: map[string][]string{"tip": {"gone"}}}

	err := requireDeliverableHistory(context.Background(), e.idx, r, "tip")
	require.ErrorIs(t, err, index.ErrUndeliveredTip, "a history that cannot be read is not vouched for")
	require.ErrorContains(t, err, "gone")
}

func TestDeliverableHistory_UnreadableCommitFailsClosed(t *testing.T) {
	e, _ := pendingContentFixture(t)
	require.Error(t, requireDeliverableHistory(context.Background(), e.idx, errReader{}, "tip"))
}

type errReader struct{}

func (errReader) Parents(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("disk gone")
}
