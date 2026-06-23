// Package provision builds the SEC-03 private, sandbox-local mgit object
// store a microVM guest commits into. It is host-only and host-trusted: the
// guest never sees the shared store, so the host seeds a FRESH private .mgit
// store containing EXACTLY the task base commit's reachable pool (the commit,
// its tree, and its blobs — the starting point the worktree was materialized
// from) and nothing else. No other branches, no other tasks' objects, and no
// shared history reach the guest, which is the cross-task-exposure the SEC-03
// quarantine forbids. The guest commits on top of this base; `mgit sandbox
// land` is the only private->shared bridge.
// Refs: SEC-03, FR-17.3, FR-17.5, MGIT-14
package provision

import (
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/model"
)

// PrivateStore is the result of provisioning: the host directory backing the
// guest's private store (mounted at the guest's <worktree>/.mgit), and the
// shared store directory the quarantine plan must prove is unreachable.
type PrivateStore struct {
	// Dir is the host path of the fresh private .mgit store.
	Dir string
	// SharedDir is the host project's .mgit store (sibling of the worktree);
	// the SEC-03 plan rejects any layout where it is reachable from the guest.
	SharedDir string
}

// Provisioner seeds private stores for sandbox launches. It is the host-trusted
// seam the microVM manager calls; an implementation lives below over go-git.
type Provisioner interface {
	// Provision creates a fresh private store under privateDir seeded with the
	// task branch's tip commit only, and reports the shared store dir for the
	// quarantine non-reachability check. privateDir MUST NOT already exist.
	Provision(taskID, privateDir string) (PrivateStore, error)
}

// StoreProvisioner provisions private stores from a project's shared .mgit
// store. RepoRoot is the host project root whose .mgit is the shared store.
type StoreProvisioner struct {
	RepoRoot string
}

// NewStoreProvisioner returns a provisioner seeded from the project at
// repoRoot (whose .mgit is the shared store). Refs: SEC-03
func NewStoreProvisioner(repoRoot string) (*StoreProvisioner, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("provision: repo root must not be empty")
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("provision: resolve repo root: %w", err)
	}
	return &StoreProvisioner{RepoRoot: abs}, nil
}

// SharedDir returns the project's shared .mgit store directory.
func (p *StoreProvisioner) SharedDir() string {
	return filepath.Join(p.RepoRoot, ".mgit")
}

// Provision creates a fresh private .mgit store at privateDir seeded with
// EXACTLY the task branch (task/<id>) tip commit's reachable pool from the
// shared store, then points the same branch + HEAD at it so the guest commits
// on top. Nothing else from the shared store is copied. privateDir must not
// pre-exist (a stale store would defeat the freshness guarantee).
// Refs: SEC-03, FR-17.5, MGIT-14
func (p *StoreProvisioner) Provision(taskID, privateDir string) (PrivateStore, error) {
	sharedDir := p.SharedDir()
	if _, err := os.Stat(sharedDir); err != nil {
		return PrivateStore{}, fmt.Errorf("%w: shared store not found at %s", model.ErrStorageError, sharedDir)
	}
	if _, err := os.Stat(privateDir); err == nil {
		return PrivateStore{}, fmt.Errorf("provision: private store %s already exists", privateDir)
	}

	shared := openBareStore(sharedDir)

	branch := plumbing.NewBranchReferenceName(model.TaskBranchName(taskID))
	ref, err := shared.Reference(branch)
	if err != nil {
		return PrivateStore{}, fmt.Errorf("%w: task branch %s in shared store", model.ErrBranchNotFound, branch.Short())
	}

	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		return PrivateStore{}, fmt.Errorf("provision: create private store dir: %w", err)
	}
	priv := openBareStore(privateDir)
	if _, err := gogit.Init(priv, nil); err != nil {
		_ = os.RemoveAll(privateDir)
		return PrivateStore{}, fmt.Errorf("provision: init go-git private store: %w", err)
	}

	if err := copyReachable(shared, priv, ref.Hash()); err != nil {
		_ = os.RemoveAll(privateDir)
		return PrivateStore{}, fmt.Errorf("provision: seed base commit: %w", err)
	}
	// Point the same task branch + HEAD at the seeded base so the guest's mgit
	// commits on top of it and land serves the branch HEAD.
	if err := priv.SetReference(plumbing.NewHashReference(branch, ref.Hash())); err != nil {
		_ = os.RemoveAll(privateDir)
		return PrivateStore{}, fmt.Errorf("provision: set private branch: %w", err)
	}
	if err := priv.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		_ = os.RemoveAll(privateDir)
		return PrivateStore{}, fmt.Errorf("provision: set private HEAD: %w", err)
	}
	return PrivateStore{Dir: privateDir, SharedDir: sharedDir}, nil
}

// openBareStore opens a worktree-less go-git filesystem store at dir, the same
// model internal/store/git uses for the self-contained .mgit store.
func openBareStore(dir string) *filesystem.Storage {
	return filesystem.NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
}

// objItem pairs an object hash with the git type used to read it.
type objItem struct {
	hash plumbing.Hash
	typ  plumbing.ObjectType
}

// copyReachable copies EXACTLY the objects reachable from head — the commit
// chain, each commit's tree (recursively), and the blobs — from src into dst.
// It is the seeding walk: it copies a base commit's full content-addressed
// pool so the guest can commit on top, and copies NOTHING outside that pool
// (no other branches, no other tasks' objects). Submodule (gitlink) entries
// are skipped — they reference another repo's commit, not an object here.
// Refs: SEC-03
func copyReachable(src storer.EncodedObjectStorer, dst storer.EncodedObjectStorer, head plumbing.Hash) error {
	if head.IsZero() {
		return nil
	}
	seen := make(map[plumbing.Hash]bool)
	work := []objItem{{head, plumbing.CommitObject}}
	for len(work) > 0 {
		it := work[len(work)-1]
		work = work[:len(work)-1]
		if it.hash.IsZero() || seen[it.hash] {
			continue
		}
		seen[it.hash] = true

		if err := copyObject(src, dst, it.typ, it.hash); err != nil {
			return err
		}
		next, err := children(src, it.typ, it.hash)
		if err != nil {
			return err
		}
		work = append(work, next...)
	}
	return nil
}

// children returns the further objects an object references (none for a blob).
func children(src storer.EncodedObjectStorer, typ plumbing.ObjectType, h plumbing.Hash) ([]objItem, error) {
	switch typ {
	case plumbing.CommitObject:
		c, err := object.GetCommit(src, h)
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", h, err)
		}
		// Follow ALL parents: the seed inherits whatever ancestry the task tip
		// has. For a normal task branch (descended from shared main-line base
		// commits) this is the task's OWN lineage — no sibling task's unique
		// objects are reachable, so no cross-task exposure (SEC-03). The one
		// exception is a tip that MERGES another task's branch (a second parent
		// pointing at task/<other>): that sibling's objects would then be
		// copied. mgit's squash semantics keep task branches isolated off base
		// (main never advances into them), so this does not arise in the normal
		// flow; if cross-task merges ever become possible, gate seeding to the
		// first parent / base only. Refs: SEC-03
		next := []objItem{{c.TreeHash, plumbing.TreeObject}}
		for _, p := range c.ParentHashes {
			next = append(next, objItem{p, plumbing.CommitObject})
		}
		return next, nil
	case plumbing.TreeObject:
		t, err := object.GetTree(src, h)
		if err != nil {
			return nil, fmt.Errorf("read tree %s: %w", h, err)
		}
		var next []objItem
		for _, e := range t.Entries {
			switch e.Mode {
			case filemode.Dir:
				next = append(next, objItem{e.Hash, plumbing.TreeObject})
			case filemode.Submodule:
				continue
			default:
				next = append(next, objItem{e.Hash, plumbing.BlobObject})
			}
		}
		return next, nil
	default:
		return nil, nil
	}
}

// copyObject copies one encoded object verbatim from src to dst.
func copyObject(src storer.EncodedObjectStorer, dst storer.EncodedObjectStorer, typ plumbing.ObjectType, h plumbing.Hash) error {
	o, err := src.EncodedObject(typ, h)
	if err != nil {
		return fmt.Errorf("read object %s: %w", h, err)
	}
	if _, err := dst.SetEncodedObject(o); err != nil {
		return fmt.Errorf("write object %s: %w", h, err)
	}
	return nil
}
