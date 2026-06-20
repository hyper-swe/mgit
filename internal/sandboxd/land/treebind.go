package land

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/hyper-swe/mgit/internal/model"
)

// emptyTreeHash is git's canonical empty tree object id; a commit with no
// files may reference it (or the zero hash in degenerate cases). Either
// yields an empty landed file set without a tree object in the pool.
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// VerifyTreeBinding binds a verified commit's CLAIMED FileDiffs to the
// ACTUAL landed tree objects (SEC-06). VerifyBinding already bound the
// commit's identity, metadata and content_hash — but content_hash is
// self-consistent over the *claimed* diffs, so nothing yet proves those
// diffs describe the real tree. This recomputes the file set from the
// landed tree (resolved from the content-addressed object pool, so a guest
// cannot substitute it), validates EVERY tree-entry path (ValidateTreePath
// — a guest must not smuggle a write outside the worktree), recomputes the
// diff against the parent commit's file set, and fails the land when the
// recomputed diff does not match the claimed FileDiffs, a referenced new
// blob is absent from the pool, or a path is unsafe.
//
// parentFiles maps each path in the parent commit's tree to its blob hash
// (empty for an initial commit); the caller resolves it from the host
// store. Pure and host-side: the guest asserts nothing this trusts.
// Refs: FR-17.24, FR-17.35, SEC-06
func VerifyTreeBinding(objects []Object, c *model.Commit, parentFiles map[string]string) error {
	if c == nil {
		return fmt.Errorf("%w: nil commit", model.ErrLandVerificationFailed)
	}
	storer, err := poolStorer(objects)
	if err != nil {
		return err
	}
	landedFiles, err := landedFileSet(storer, c.TreeHash)
	if err != nil {
		return err
	}
	recomputed := diffFileSets(parentFiles, landedFiles)

	// Every new/modified blob must be present in the landed pool, or the
	// commit's tree would reference content that never landed (a dangling
	// object). Unchanged files keep their blobs in the parent store.
	for _, d := range recomputed {
		if d.NewHash == "" {
			continue
		}
		if _, err := storer.EncodedObject(plumbing.BlobObject, plumbing.NewHash(d.NewHash)); err != nil {
			return fmt.Errorf("%w: a landed blob is absent from the object pool",
				model.ErrLandVerificationFailed)
		}
	}
	if !diffsMatch(c.FileDiffs, recomputed) {
		return fmt.Errorf("%w: claimed file diffs do not match the landed tree",
			model.ErrLandVerificationFailed)
	}
	return nil
}

// poolStorer loads the content-addressed object pool into an in-memory git
// store so the landed tree can be walked. Objects are stored under their
// true computed hash (content addressing), so a guest cannot make an
// object resolve under an id it does not hash to.
func poolStorer(objects []Object) (*memory.Storage, error) {
	st := memory.NewStorage()
	for _, obj := range objects {
		typ, err := gitObjectType(obj.Type)
		if err != nil {
			return nil, err
		}
		o := st.NewEncodedObject()
		o.SetType(typ)
		o.SetSize(int64(len(obj.Data)))
		w, err := o.Writer()
		if err != nil {
			return nil, fmt.Errorf("%w: stage object: %w", model.ErrLandVerificationFailed, err)
		}
		if _, err := w.Write(obj.Data); err != nil {
			return nil, fmt.Errorf("%w: stage object: %w", model.ErrLandVerificationFailed, err)
		}
		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("%w: stage object: %w", model.ErrLandVerificationFailed, err)
		}
		if _, err := st.SetEncodedObject(o); err != nil {
			return nil, fmt.Errorf("%w: stage object: %w", model.ErrLandVerificationFailed, err)
		}
	}
	return st, nil
}

// landedFileSet walks the landed tree to its leaf files, validating every
// entry path and returning path -> blob hash. An empty/zero tree yields an
// empty set without requiring a tree object.
func landedFileSet(st *memory.Storage, treeHash string) (map[string]string, error) {
	files := make(map[string]string)
	if treeHash == "" || treeHash == emptyTreeHash || treeHash == plumbing.ZeroHash.String() {
		return files, nil
	}
	tree, err := object.GetTree(st, plumbing.NewHash(treeHash))
	if err != nil {
		return nil, fmt.Errorf("%w: landed tree %s not resolvable from the pool",
			model.ErrLandVerificationFailed, treeHash)
	}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: walk landed tree: %w", model.ErrLandVerificationFailed, err)
		}
		// Validate every entry path (files AND directory nodes): a traversal
		// could hide in a directory name.
		if err := ValidateTreePath(name); err != nil {
			return nil, err
		}
		if entry.Mode == filemode.Dir {
			continue
		}
		files[name] = entry.Hash.String()
	}
	return files, nil
}

// diffFileSets recomputes the add/modify/delete diff between the parent and
// landed file sets. mgit's own commit diffs carry no rename detection, so
// neither does this — the claimed diffs are produced the same way.
func diffFileSets(parent, landed map[string]string) []model.FileDiff {
	var diffs []model.FileDiff
	for path, newHash := range landed {
		oldHash, ok := parent[path]
		switch {
		case !ok:
			diffs = append(diffs, model.FileDiff{Path: path, Operation: model.DiffAdded, NewHash: newHash})
		case oldHash != newHash:
			diffs = append(diffs, model.FileDiff{Path: path, Operation: model.DiffModified, OldHash: oldHash, NewHash: newHash})
		}
	}
	for path, oldHash := range parent {
		if _, ok := landed[path]; !ok {
			diffs = append(diffs, model.FileDiff{Path: path, Operation: model.DiffDeleted, OldHash: oldHash})
		}
	}
	return diffs
}

// diffsMatch reports whether the claimed and recomputed diffs are the same
// multiset over the tree-derivable fields (path, operation, old/new hash).
// Hunks and binary flags are line-content detail, not tree-derivable, so
// they are not compared here; the new-blob presence check binds content.
func diffsMatch(claimed, recomputed []model.FileDiff) bool {
	if len(claimed) != len(recomputed) {
		return false
	}
	key := func(d model.FileDiff) string {
		return d.Path + "\x00" + string(d.Operation) + "\x00" + d.OldHash + "\x00" + d.NewHash
	}
	counts := make(map[string]int, len(recomputed))
	for _, d := range recomputed {
		counts[key(d)]++
	}
	for _, d := range claimed {
		counts[key(d)]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
