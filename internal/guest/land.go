package guest

import (
	"errors"
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/hyper-swe/mgit/internal/landwire"
)

// ServeLandHead opens the worktree's git repository, resolves its checked-out
// HEAD (the task branch the sandbox is bound to), and streams every object
// reachable from it to w. It is the connection-serving entry point for the
// guest land channel: the host dials, this serves the pool, the connection
// closes. An empty repository (unborn HEAD) serves nothing — there is
// nothing to land. PURE TRANSPORT: it never inspects or asserts provenance
// (SEC-01). Refs: FR-17.5, SEC-01
func ServeLandHead(repoPath string, w io.Writer) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("guest land: open repo %s: %w", repoPath, err)
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
