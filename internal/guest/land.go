package guest

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/landwire"
)

// guestStoreName is the worktree-relative directory the guest's private,
// sandbox-local mgit object store is mounted at. It mirrors the host
// quarantine binding (quarantine.guestStoreName) and the store layout the
// rest of mgit standardizes on post-MGIT-14: mgit's store lives in .mgit/,
// never a .git at the worktree root. The guest commits into this private
// store and land streams its reachable pool back to the host. Keeping the
// constant here (not importing the host-only quarantine package from the
// guest binary) keeps the in-guest code free of host-side dependencies.
// Refs: SEC-03, MGIT-14
const guestStoreName = ".mgit"

// ServeLandHead opens the worktree's PRIVATE sandbox-local mgit object store
// at <worktreePath>/.mgit, resolves its HEAD (the task branch the sandbox is
// bound to), and streams every object reachable from it to w. It is the
// connection-serving entry point for the guest land channel: the host dials,
// this serves the pool, the connection closes. An empty store (unborn HEAD)
// serves nothing — there is nothing to land. PURE TRANSPORT: it never
// inspects or asserts provenance (SEC-01).
//
// The store is opened as a BARE, worktree-less go-git store
// (osfs+filesystem.NewStorage+gogit.Open(storage,nil)) — the same model
// internal/store/git.Open uses — NOT gogit.PlainOpen of a .git: per SEC-03
// the guest only ever sees working-tree files plus this private .mgit store
// the host bound; there is no host shared .mgit and no project .git reachable
// from inside the guest. Refs: FR-17.5, SEC-01, SEC-03, MGIT-14
func ServeLandHead(worktreePath string, w io.Writer) error {
	mgitPath := filepath.Join(worktreePath, guestStoreName)
	dotFS := osfs.New(mgitPath)
	storage := filesystem.NewStorage(dotFS, cache.NewObjectLRUDefault())
	repo, err := gogit.Open(storage, nil)
	if err != nil {
		return fmt.Errorf("guest land: open private store %s: %w", mgitPath, err)
	}
	head, err := repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil // unborn branch: nothing reachable to land
		}
		return fmt.Errorf("guest land: resolve HEAD: %w", err)
	}
	return StreamReachable(repo.Storer, head.Hash(), w)
}

// landItem is one object to serve, with the git type used to read it.
type landItem struct {
	hash plumbing.Hash
	typ  plumbing.ObjectType
}

// StreamReachable serves a task branch's git objects to the host land
// channel: every object reachable from head — the commit chain, each
// commit's tree (recursively), and the blobs — written exactly once as a
// landwire frame to w. It is PURE TRANSPORT (SEC-01): it frames raw
// content-addressed bytes and makes no integrity or provenance claim; the
// host re-derives and verifies everything it imports. The guest holds no
// signing key. A missing or undecodable object fails the stream (the host
// would reject a truncated pool anyway). Refs: FR-17.5, SEC-01
func StreamReachable(src storer.EncodedObjectStorer, head plumbing.Hash, w io.Writer) error {
	if head.IsZero() {
		return nil // an empty branch has nothing to land
	}
	seen := make(map[plumbing.Hash]bool)
	work := []landItem{{head, plumbing.CommitObject}}
	for len(work) > 0 {
		it := work[len(work)-1]
		work = work[:len(work)-1]
		if it.hash.IsZero() || seen[it.hash] {
			continue
		}
		seen[it.hash] = true

		next, err := serveObject(src, it, w)
		if err != nil {
			return err
		}
		work = append(work, next...)
	}
	return nil
}

// serveObject frames one object and returns the further objects it
// references (none for a blob).
func serveObject(src storer.EncodedObjectStorer, it landItem, w io.Writer) ([]landItem, error) {
	switch it.typ {
	case plumbing.CommitObject:
		return serveCommit(src, it.hash, w)
	case plumbing.TreeObject:
		return serveTree(src, it.hash, w)
	case plumbing.BlobObject:
		return nil, serveRaw(src, plumbing.BlobObject, landwire.ObjBlob, it.hash, w)
	default:
		return nil, fmt.Errorf("guest land: unexpected object type %s", it.typ)
	}
}

// serveCommit frames a commit and returns its tree and parents.
func serveCommit(src storer.EncodedObjectStorer, h plumbing.Hash, w io.Writer) ([]landItem, error) {
	c, err := object.GetCommit(src, h)
	if err != nil {
		return nil, fmt.Errorf("guest land: read commit %s: %w", h, err)
	}
	if err := serveRaw(src, plumbing.CommitObject, landwire.ObjCommit, h, w); err != nil {
		return nil, err
	}
	next := []landItem{{c.TreeHash, plumbing.TreeObject}}
	for _, p := range c.ParentHashes {
		next = append(next, landItem{p, plumbing.CommitObject})
	}
	return next, nil
}

// serveTree frames a tree and returns its entries: sub-trees as trees,
// files as blobs, gitlinks skipped (they reference another repo's commit,
// not an object in this pool — the host tree binding ignores them too).
func serveTree(src storer.EncodedObjectStorer, h plumbing.Hash, w io.Writer) ([]landItem, error) {
	tree, err := object.GetTree(src, h)
	if err != nil {
		return nil, fmt.Errorf("guest land: read tree %s: %w", h, err)
	}
	if err := serveRaw(src, plumbing.TreeObject, landwire.ObjTree, h, w); err != nil {
		return nil, err
	}
	var next []landItem
	for _, e := range tree.Entries {
		switch e.Mode {
		case filemode.Dir:
			next = append(next, landItem{e.Hash, plumbing.TreeObject})
		case filemode.Submodule:
			continue
		default:
			next = append(next, landItem{e.Hash, plumbing.BlobObject})
		}
	}
	return next, nil
}

// serveRaw frames one object's canonical payload bytes under the given land
// type tag.
func serveRaw(src storer.EncodedObjectStorer, typ plumbing.ObjectType, tag byte, h plumbing.Hash, w io.Writer) error {
	o, err := src.EncodedObject(typ, h)
	if err != nil {
		return fmt.Errorf("guest land: read object %s: %w", h, err)
	}
	r, err := o.Reader()
	if err != nil {
		return fmt.Errorf("guest land: open object %s: %w", h, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("guest land: read object body %s: %w", h, err)
	}
	return landwire.WriteFrame(w, tag, data)
}
