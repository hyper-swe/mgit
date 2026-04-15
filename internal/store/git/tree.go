package git

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// TreeEntry represents a file entry in a git tree.
// Refs: FR-11, MGIT-2.2.3
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Hash string `json:"hash"`
}

// TreeStore manages tree objects in the go-git store.
// Refs: FR-11, MGIT-2.2.3
type TreeStore struct {
	repo *Repository
}

// NewTreeStore creates a TreeStore backed by the given Repository.
func NewTreeStore(repo *Repository) *TreeStore {
	return &TreeStore{repo: repo}
}

// BuildTree creates a tree object from file diffs by modifying
// the current HEAD tree. Returns the new tree's SHA-1 hash.
// Refs: FR-11
func (ts *TreeStore) BuildTree(_ context.Context, diffs []model.FileDiff) (string, error) {
	goRepo := ts.repo.repo

	// Get current HEAD tree as base
	headRef, err := goRepo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	headCommit, err := goRepo.CommitObject(headRef.Hash())
	if err != nil {
		return "", fmt.Errorf("get HEAD commit: %w", err)
	}
	tree, err := headCommit.Tree()
	if err != nil {
		return "", fmt.Errorf("get HEAD tree: %w", err)
	}

	// Build new tree entries based on current tree + diffs
	entries := make([]object.TreeEntry, 0, len(tree.Entries)+len(diffs))

	// Copy existing entries (excluding deleted/modified paths)
	modifiedPaths := make(map[string]bool)
	for _, d := range diffs {
		modifiedPaths[d.Path] = true
	}
	for _, e := range tree.Entries {
		if !modifiedPaths[e.Name] {
			entries = append(entries, e)
		}
	}

	// Add new/modified entries from diffs
	for _, d := range diffs {
		if d.Operation == model.DiffDeleted {
			continue
		}
		entries = append(entries, object.TreeEntry{
			Name: d.Path,
			Mode: filemode.Regular,
			Hash: plumbing.NewHash(d.NewHash),
		})
	}

	// Sort entries by name (go-git requires sorted tree entries)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	// Create new tree object
	newTree := &object.Tree{Entries: entries}
	obj := ts.repo.repo.Storer.NewEncodedObject()
	if err := newTree.Encode(obj); err != nil {
		return "", fmt.Errorf("encode tree: %w", err)
	}
	hash, err := ts.repo.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return "", fmt.Errorf("store tree: %w", err)
	}

	return hash.String(), nil
}

// GetTree retrieves a tree by its SHA-1 hash.
// Refs: FR-11
func (ts *TreeStore) GetTree(_ context.Context, hash string) ([]TreeEntry, error) {
	h := plumbing.NewHash(hash)
	tree, err := object.GetTree(ts.repo.repo.Storer, h)
	if err != nil {
		return nil, fmt.Errorf("get tree %s: %w", hash, err)
	}

	entries := make([]TreeEntry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		entries = append(entries, TreeEntry{
			Path: e.Name,
			Mode: e.Mode.String(),
			Hash: e.Hash.String(),
		})
	}
	return entries, nil
}

// TraverseTree recursively lists all entries in a tree.
// Refs: FR-11
func (ts *TreeStore) TraverseTree(_ context.Context, hash string) ([]TreeEntry, error) {
	h := plumbing.NewHash(hash)
	tree, err := object.GetTree(ts.repo.repo.Storer, h)
	if err != nil {
		return nil, fmt.Errorf("get tree %s: %w", hash, err)
	}

	var entries []TreeEntry
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()

	for {
		name, entry, walkErr := walker.Next()
		if walkErr != nil {
			break
		}
		entries = append(entries, TreeEntry{
			Path: name,
			Mode: entry.Mode.String(),
			Hash: entry.Hash.String(),
		})
	}

	return entries, nil
}
