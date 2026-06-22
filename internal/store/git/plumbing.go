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

// writeFlatTree encodes a single tree level (its direct entries) as a tree
// object and stores it. Entries are sorted by name as go-git requires. Callers
// that have hierarchical paths must use writeNestedTree instead.
func writeFlatTree(st storer.EncodedObjectStorer, entries []object.TreeEntry) (plumbing.Hash, error) {
	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

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
// slash-separated project-relative paths → blob hashes, recursively creating
// and storing one tree object per directory level. Returns the root tree hash.
// A git tree cannot hold a slash in an entry name, so paths like
// "sub/dir/nested.txt" must become nested `sub` → `dir` → `nested.txt` trees;
// this function is the single place that materializes that hierarchy.
func writeNestedTree(st storer.EncodedObjectStorer, files map[string]plumbing.Hash) (plumbing.Hash, error) {
	// fileChildren: immediate file entries at this level (name → blob hash).
	fileChildren := make(map[string]plumbing.Hash)
	// dirChildren: subtrees at this level (dir name → its flattened files).
	dirChildren := make(map[string]map[string]plumbing.Hash)

	for path, blob := range files {
		name, rest, nested := strings.Cut(path, "/")
		if !nested {
			fileChildren[name] = blob
			continue
		}
		if dirChildren[name] == nil {
			dirChildren[name] = make(map[string]plumbing.Hash)
		}
		dirChildren[name][rest] = blob
	}

	entries := make([]object.TreeEntry, 0, len(fileChildren)+len(dirChildren))
	for name, blob := range fileChildren {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob})
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
// slash-separated paths → blob hashes for every file (blob) it contains.
func flattenTree(tree *object.Tree) (map[string]plumbing.Hash, error) {
	out := make(map[string]plumbing.Hash)
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
		out[name] = entry.Hash
	}
	return out, nil
}
