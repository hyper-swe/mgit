package git

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// This file is the SINGLE source of the low-level plumbing primitives that
// build commits without a go-git worktree or .git index. mgit's .mgit/ store
// is opened bare (worktree=nil) so go-git never writes a `.git` gitfile at the
// project root; every object is therefore constructed and stored directly via
// the object storer. BuildTree (tree.go), CreateCommit (commit.go), the merge
// commit (merge.go) and worktree checkout (worktree.go) all funnel through
// these helpers so the plumbing logic lives in one place.
// Refs: MGIT-14, ADR-001 (amendment 2026-06-22)

// blobEntry pairs a stored blob's hash with the git file mode that should be
// recorded for it in a tree (and restored on checkout). Carrying the mode
// alongside the hash through the staging→tree→disk pipeline preserves git's
// regular/executable/symlink distinction, so mgit's tree hash equals git's
// for a mode-varied tree (the reproducible-provenance guarantee).
// Refs: MGIT-14.7
type blobEntry struct {
	hash plumbing.Hash
	mode filemode.FileMode
}

// advanceBranchRefCAS atomically advances branch from oldHash to newHash via
// go-git's CheckAndSetReference: the update is applied only if the stored value
// still matches oldHash. A concurrent move (the stored value already advanced)
// fails loudly instead of silently orphaning the losing commit. This is the
// single place branch tips are advanced after a new commit/merge.
// Refs: MGIT-14.7 (#5)
func advanceBranchRefCAS(st storer.ReferenceStorer, branch plumbing.ReferenceName, newHash, oldHash plumbing.Hash) error {
	newRef := plumbing.NewHashReference(branch, newHash)
	oldRef := plumbing.NewHashReference(branch, oldHash)
	if err := st.CheckAndSetReference(newRef, oldRef); err != nil {
		return fmt.Errorf("advance ref %s: %w", branch.Short(), err)
	}
	return nil
}

// writeBlob stores raw file content as a git blob object and returns its
// SHA-1 hash. It is idempotent: the object store is content-addressed.
func writeBlob(st storer.EncodedObjectStorer, content []byte) (plumbing.Hash, error) {
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob writer: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("write blob: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close blob writer: %w", err)
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store blob: %w", err)
	}
	return h, nil
}

// treeEntryLess orders entries in git's canonical tree order: names compared
// byte-wise, but a directory (tree) entry compares as if its name had a
// trailing "/". This differs from a plain name sort whenever a directory name
// is a prefix-relative sibling of a file (e.g. dir "land" vs file "land.go":
// git puts "land.go" before "land/"). go-git's tree encoder rejects entries
// not in this exact order ("entries in tree are not sorted"). Refs: MGIT-14
func treeEntryLess(a, b object.TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == filemode.Dir {
		an += "/"
	}
	if b.Mode == filemode.Dir {
		bn += "/"
	}
	return an < bn
}

// writeFlatTree encodes a single tree level (its direct entries) as a tree
// object and stores it. Entries are sorted in git's canonical tree order (see
// treeEntryLess) as go-git's encoder requires. Callers with hierarchical paths
// must use writeNestedTree instead.
func writeFlatTree(st storer.EncodedObjectStorer, entries []object.TreeEntry) (plumbing.Hash, error) {
	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeEntryLess(sorted[i], sorted[j]) })

	tree := &object.Tree{Entries: sorted}
	obj := st.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree: %w", err)
	}
	return h, nil
}

// writeNestedTree builds a full hierarchical tree from a flat map of
// slash-separated project-relative paths → blobEntry (hash + git mode),
// recursively creating and storing one tree object per directory level.
// Returns the root tree hash. A git tree cannot hold a slash in an entry name,
// so paths like "sub/dir/nested.txt" must become nested `sub` → `dir` →
// `nested.txt` trees; this is the single place that materializes that
// hierarchy. Each leaf carries its own mode (100644/100755/120000) so the tree
// hash is git-faithful for mode-varied trees. Refs: MGIT-14.7 (#2, #3)
func writeNestedTree(st storer.EncodedObjectStorer, files map[string]blobEntry) (plumbing.Hash, error) {
	// fileChildren: immediate file entries at this level (name → blob+mode).
	fileChildren := make(map[string]blobEntry)
	// dirChildren: subtrees at this level (dir name → its flattened files).
	dirChildren := make(map[string]map[string]blobEntry)

	for path, entry := range files {
		name, rest, nested := strings.Cut(path, "/")
		if !nested {
			fileChildren[name] = entry
			continue
		}
		if dirChildren[name] == nil {
			dirChildren[name] = make(map[string]blobEntry)
		}
		dirChildren[name][rest] = entry
	}

	entries := make([]object.TreeEntry, 0, len(fileChildren)+len(dirChildren))
	for name, e := range fileChildren {
		entries = append(entries, object.TreeEntry{Name: name, Mode: e.mode, Hash: e.hash})
	}
	for name, sub := range dirChildren {
		subHash, err := writeNestedTree(st, sub)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: subHash})
	}
	return writeFlatTree(st, entries)
}

// commitParams carries the inputs needed to build a commit object via plumbing.
type commitParams struct {
	tree     plumbing.Hash
	parents  []plumbing.Hash
	message  string
	authorAt object.Signature
}

// writeCommit encodes and stores a commit object pointing at the given tree
// and parents, with author == committer == the supplied signature (mgit fixes
// committer to the author for deterministic, audit-stable commits). Returns
// the commit's SHA-1 hash.
func writeCommit(st storer.EncodedObjectStorer, p commitParams) (plumbing.Hash, error) {
	commit := &object.Commit{
		Author:       p.authorAt,
		Committer:    p.authorAt,
		Message:      p.message,
		TreeHash:     p.tree,
		ParentHashes: p.parents,
	}
	obj := st.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode commit: %w", err)
	}
	h, err := st.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit: %w", err)
	}
	return h, nil
}

// emptyTree stores and returns the canonical empty tree (no entries). Used for
// the initial commit so HEAD is valid in a freshly initialized store.
func emptyTree(st storer.EncodedObjectStorer) (plumbing.Hash, error) {
	return writeFlatTree(st, nil)
}

// flattenTree walks a tree recursively and returns a flat map of full
// slash-separated paths → blobEntry (hash + mode) for every file (blob and
// symlink) it contains. The mode is preserved so checkout can restore the
// executable bit and recreate symlinks. Refs: MGIT-14.7 (#2, #3)
func flattenTree(tree *object.Tree) (map[string]blobEntry, error) {
	out := make(map[string]blobEntry)
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if err != nil {
			break
		}
		if entry.Mode == filemode.Dir {
			continue
		}
		out[name] = blobEntry{hash: entry.Hash, mode: entry.Mode}
	}
	return out, nil
}
