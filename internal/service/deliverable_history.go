package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/store/index"
)

// commitReader reads one commit's parents, all of them: a merge commit has
// more than one, and a walk that follows only the first would skip a branch
// merged into the history.
type commitReader interface {
	Parents(ctx context.Context, hash string) ([]string, error)
}

// requireDeliverableHistory refuses a commit that is, or is built on, a sandbox
// land that has not been delivered to the host. It walks every parent from tip
// with a visited set, so cost is bounded by the history that exists and a
// merge is followed down each side. A walk does not go below a delivered LAND
// row (history below it was admitted with it); it ends at the root. A
// host-origin row stops nothing: a host commit can sit on guest bytes. A commit
// that cannot be read is "cannot tell" and refuses. With no undelivered land
// commit recorded it does not read a single commit. Refs: MGIT-323.5
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
	seen := map[string]struct{}{}
	for queue := []string{tip}; len(queue) > 0; {
		hash := queue[0]
		queue = queue[1:]
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		if _, bad := undelivered[hash]; bad {
			if hash == tip {
				return fmt.Errorf("%w %s: it came from a sandbox land and has not been delivered to the host", index.ErrUndeliveredTip, hash)
			}
			return fmt.Errorf("%w %s: the commit you named is built on it, and it came from a sandbox land that has not been delivered to the host", index.ErrUndeliveredTip, hash)
		}
		if _, ok := delivered[hash]; ok {
			continue
		}
		parents, err := commits.Parents(ctx, hash)
		if err != nil {
			return fmt.Errorf("%w: the history of %s cannot be checked: commit %s could not be read: %v", index.ErrUndeliveredTip, tip, hash, err)
		}
		queue = append(queue, parents...)
	}
	return nil
}
