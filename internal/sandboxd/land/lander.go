package land

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// ObjectImporter writes verified git objects into the host object store.
// Imports MUST be idempotent (git objects are content-addressed), so
// objects written before an aborted land are harmless orphans, never
// corruption — that property is what lets land import objects before the
// atomic task_commits append. Refs: FR-17.5
type ObjectImporter interface {
	ImportObjects(ctx context.Context, objs []Object) error
}

// CommitAppender atomically appends a batch of task_commits rows — every
// row commits or none do (index.Store.AppendTaskCommits). Refs: FR-17.5
type CommitAppender interface {
	AppendTaskCommits(ctx context.Context, ins []index.TaskCommitInsert) error
}

// Brancher fast-forwards a task branch to a commit. Implementations MUST
// be append-only: a non-fast-forward (history rewrite) is refused — land
// never rewrites the task branch (FR-17.5). Refs: FR-17.5
type Brancher interface {
	FastForward(ctx context.Context, taskID, commitHash string) error
}

// LandedCommit is one host-verified commit ready to persist: the commit
// metadata, its sandbox provenance from the require_sandbox gate (nil =
// NULL, the unsandboxed gap), and its append position on the task branch.
// The git objects are not per-commit — they are one shared pool for the
// whole land, passed to Land separately (see Lander.Land).
type LandedCommit struct {
	Commit    *model.Commit
	SandboxID *string
	Position  int
}

// Lander persists a batch of already-verified commits atomically.
type Lander struct {
	importer ObjectImporter
	appender CommitAppender
	brancher Brancher
}

// NewLander wires a Lander.
func NewLander(importer ObjectImporter, appender CommitAppender, brancher Brancher) *Lander {
	return &Lander{importer: importer, appender: appender, brancher: brancher}
}

// Land persists a verified batch all-or-nothing (FR-17.5): it imports the
// land's object pool, then appends every task_commits row in one
// serialized transaction, then fast-forwards the task branch append-only.
//
// The objects are one content-addressed pool for the whole land (a blob
// or tree shared by several commits is one object, not one per commit),
// so they are imported once rather than partitioned per commit. Ordering
// gives the atomicity guarantee: the pool import comes first and is
// idempotent, so a failure there aborts before a single task_commits row
// exists (leaving only harmless orphan objects, no partial land); the
// append is the all-or-nothing commit point; the append-only fast-forward
// publishes the branch last. The append records the host receive-time
// (the store's clock); the guest's own timestamp stays advisory inside
// the git object (SEC-11, FR-17.28). Refs: FR-17.5, SEC-11
func (l *Lander) Land(ctx context.Context, taskID string, pool []Object, commits []LandedCommit) error {
	if len(commits) == 0 {
		return nil
	}
	if err := l.importer.ImportObjects(ctx, pool); err != nil {
		return fmt.Errorf("land: import objects: %w", err)
	}

	rows := make([]index.TaskCommitInsert, 0, len(commits))
	for _, c := range commits {
		var sandboxID string // empty → stored as NULL (unsandboxed gap)
		if c.SandboxID != nil {
			sandboxID = *c.SandboxID
		}
		rows = append(rows, index.TaskCommitInsert{
			TaskID:      taskID,
			CommitHash:  c.Commit.CommitID,
			ContentHash: c.Commit.ContentHash,
			AgentID:     c.Commit.AgentID,
			Position:    c.Position,
			SandboxID:   sandboxID,
		})
	}
	if err := l.appender.AppendTaskCommits(ctx, rows); err != nil {
		return fmt.Errorf("land: append task_commits: %w", err)
	}

	last := commits[len(commits)-1].Commit.CommitID
	if err := l.brancher.FastForward(ctx, taskID, last); err != nil {
		return fmt.Errorf("land: fast-forward %s: %w", taskID, err)
	}
	return nil
}
