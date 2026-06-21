package land

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/hyper-swe/mgit/internal/model"
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

// PoolAwareParentResolver resolves a parent commit's file set from the host
// store OR, for a parent that is itself a new commit in the land batch (an
// intra-batch parent not yet imported), from the registered land pool. This
// lets a multi-commit chain land atomically: the second commit's parent is
// the first commit, which lives only in the pool until the whole batch
// persists. It satisfies the service.ParentTreeResolver port and wraps the
// host resolver for base-history parents. The land service registers the
// pulled pool around one orchestrated land; resolution is keyed by the
// globally unique commit id, so concurrent lands of different sandboxes do
// not collide. Refs: SEC-06, FR-17.24, FR-17.5
type PoolAwareParentResolver struct {
	host *HostParentTreeResolver

	mu       sync.Mutex
	byCommit map[string][]Object // intra-batch commit id -> its land pool
}

// NewPoolAwareParentResolver wraps a host resolver with a pool registry.
func NewPoolAwareParentResolver(host *HostParentTreeResolver) *PoolAwareParentResolver {
	return &PoolAwareParentResolver{host: host, byCommit: make(map[string][]Object)}
}

// Register records a land pool so its own new commits can serve as
// intra-batch parents, returning the registered commit ids for Deregister.
// A duplicate commit id is a schema violation. Refs: FR-17.5
func (r *PoolAwareParentResolver) Register(pool []Object) ([]string, error) {
	ids, err := commitIDs(pool)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.byCommit[id] = pool
	}
	return ids, nil
}

// Deregister drops the registry entries for a finished land.
func (r *PoolAwareParentResolver) Deregister(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		delete(r.byCommit, id)
	}
}

// ParentFileSet resolves an intra-batch parent from its registered pool, and
// otherwise from the host store. Refs: SEC-06, FR-17.24
func (r *PoolAwareParentResolver) ParentFileSet(ctx context.Context, parentCommitID string) (map[string]string, error) {
	r.mu.Lock()
	pool, ok := r.byCommit[parentCommitID]
	r.mu.Unlock()
	if ok {
		return poolCommitFileSet(pool, parentCommitID)
	}
	return r.host.ParentFileSet(ctx, parentCommitID)
}

// commitIDs returns the host-computed git ids of every commit object in a
// pool, rejecting a duplicate (the same commit served twice).
func commitIDs(pool []Object) ([]string, error) {
	seen := make(map[string]bool)
	var ids []string
	for _, o := range pool {
		if o.Type != ObjCommit {
			continue
		}
		id := plumbing.ComputeHash(plumbing.CommitObject, o.Data).String()
		if seen[id] {
			return nil, fmt.Errorf("%w: duplicate commit object %s", model.ErrLandVerificationFailed, id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// poolCommitFileSet returns the leaf file set (path -> blob hash) of a
// commit that lives in the given pool, resolving its tree from the pool's
// content-addressed objects (so an intra-batch parent binds to real bytes).
func poolCommitFileSet(pool []Object, commitID string) (map[string]string, error) {
	for _, o := range pool {
		if o.Type != ObjCommit {
			continue
		}
		if plumbing.ComputeHash(plumbing.CommitObject, o.Data).String() != commitID {
			continue
		}
		c, err := gitstore.CommitFromObjectData(o.Data)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", model.ErrLandVerificationFailed, err)
		}
		storer, err := poolStorer(pool)
		if err != nil {
			return nil, err
		}
		return landedFileSet(storer, c.TreeHash)
	}
	return nil, fmt.Errorf("%w: intra-batch parent %s absent from pool", model.ErrLandVerificationFailed, commitID)
}
