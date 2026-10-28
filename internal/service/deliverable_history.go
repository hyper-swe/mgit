package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// maxHistoryWalk bounds how many parents one admission check follows. A chain
// longer than this refuses instead of passing unchecked. Refs: MGIT-323.5
const maxHistoryWalk = 2000

// commitReader reads one commit, for its parent.
type commitReader interface {
	GetCommit(ctx context.Context, hash string) (*model.Commit, error)
}

// requireDeliverableHistory refuses a commit that is, or is built on, a sandbox
// land that has not been delivered to the host. It walks the parent chain from
// tip and stops at the nearest delivered LAND row (history below it was
// admitted with it) or at the root. A host-origin row stops nothing: a host
// commit can sit on top of guest bytes, so only a delivered land row vouches
// for what lies below. With no undelivered land commit recorded it does not
// read a single commit. Refs: MGIT-323.5
func requireDeliverableHistory(ctx context.Context, idx *index.Store, commits commitReader, tip string) error {
	undelivered, delivered, err := idx.LandAdmissions(ctx)
	if err != nil {
		return fmt.Errorf("read content ledger: %w", err)
	}
	if len(undelivered) == 0 {
		return nil
	}
	if commits == nil {
		return fmt.Errorf("%w: no commit reader is wired, so the history of %s cannot be checked", index.ErrUndeliveredTip, tip)
	}
	for hash, steps := tip, 0; hash != ""; steps++ {
		if _, bad := undelivered[hash]; bad {
			if hash == tip {
				return fmt.Errorf("%w %s: it came from a sandbox land and has not been delivered to the host", index.ErrUndeliveredTip, hash)
			}
			return fmt.Errorf("%w %s: %s is built on it, and it came from a sandbox land that has not been delivered to the host", index.ErrUndeliveredTip, hash, tip)
		}
		if _, ok := delivered[hash]; ok {
			return nil
		}
		if steps >= maxHistoryWalk {
			return fmt.Errorf("%w: %s has more than %d commits of history to check, so it is refused rather than passed unchecked", index.ErrUndeliveredTip, tip, maxHistoryWalk)
		}
		c, err := commits.GetCommit(ctx, hash)
		if err != nil {
			if errors.Is(err, model.ErrCommitNotFound) {
				return nil
			}
			return fmt.Errorf("walk history of %s at %s: %w", tip, hash, err)
		}
		hash = c.ParentID
	}
	return nil
}
