package land

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// HostParentTreeResolver resolves a parent commit's full file set
// (path -> blob hash) from the host shared store, so the SEC-06 tree
// binding can recompute a landed commit's diff against real, host-anchored
// content rather than anything the guest asserts. It satisfies the
// service.ParentTreeResolver port. An empty parent id yields the empty set
// (an initial commit). Host-side; read-only. Refs: SEC-06, FR-17.24
type HostParentTreeResolver struct {
	commits *gitstore.CommitStore
	trees   *gitstore.TreeStore
}

// NewHostParentTreeResolver wires the resolver to a host repository.
func NewHostParentTreeResolver(repo *gitstore.Repository) *HostParentTreeResolver {
	return &HostParentTreeResolver{
		commits: gitstore.NewCommitStore(repo),
		trees:   gitstore.NewTreeStore(repo),
	}
}

// ParentFileSet returns the parent commit's leaf files as path -> blob hash.
// Directory nodes are excluded so the result matches the landed file set the
// tree binding recomputes from the pool. An empty or zero parent id is the
// initial-commit case and yields an empty set. Refs: SEC-06, FR-17.24
func (r *HostParentTreeResolver) ParentFileSet(ctx context.Context, parentCommitID string) (map[string]string, error) {
	files := make(map[string]string)
	if parentCommitID == "" || parentCommitID == plumbing.ZeroHash.String() {
		return files, nil
	}
	c, err := r.commits.GetCommit(ctx, parentCommitID)
	if err != nil {
		return nil, fmt.Errorf("resolve parent commit %s: %w", parentCommitID, err)
	}
	entries, err := r.trees.TraverseTree(ctx, c.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("traverse parent tree %s: %w", c.TreeHash, err)
	}
	for _, e := range entries {
		if e.Mode == filemode.Dir.String() {
			continue
		}
		files[e.Path] = e.Hash
	}
	return files, nil
}
