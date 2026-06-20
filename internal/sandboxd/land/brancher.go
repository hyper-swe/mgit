package land

import (
	"context"

	"github.com/hyper-swe/mgit/internal/model"
)

// fastForwarder advances a named branch to a commit. The host git
// MergeStore (store/git.MergeStore.FastForward) satisfies it; injected so
// the brancher is testable without a real repository and so land does not
// import a concrete store type it does not otherwise need.
type fastForwarder interface {
	FastForward(ctx context.Context, branchName, commitHash string) error
}

// StoreBrancher is the concrete land Brancher: it fast-forwards the task's
// branch to a landed commit. It resolves the task to its canonical branch
// name via model.TaskBranchName — the SAME convention branch creation and
// rollback use — so a landed commit advances exactly the task branch the
// rest of mgit reads, never a divergent ref. Append-only: the underlying
// fast-forward refuses a non-fast-forward. Refs: FR-17.5
type StoreBrancher struct {
	ff fastForwarder
}

// NewStoreBrancher wires the brancher to a host fast-forwarder
// (store/git.MergeStore).
func NewStoreBrancher(ff fastForwarder) *StoreBrancher {
	return &StoreBrancher{ff: ff}
}

// FastForward advances the task's branch (task/<id>) to commitHash.
func (b *StoreBrancher) FastForward(ctx context.Context, taskID, commitHash string) error {
	return b.ff.FastForward(ctx, model.TaskBranchName(taskID), commitHash)
}
