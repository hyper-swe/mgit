package git

import (
	"context"
	"fmt"
	"path/filepath"

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

// BuildTree creates a tree object by applying the given file diffs to the
// current HEAD tree, built entirely via plumbing (nested subtrees are created
// for slash-separated paths). Added/modified diffs set the path's blob to
// NewHash with a regular file mode (FileDiff carries no mode); deleted diffs
// remove the path. Existing HEAD entries retain their recorded mode. Returns
// the new tree's SHA-1 hash. Refs: FR-11, MGIT-14.3, MGIT-14.7
func (ts *TreeStore) BuildTree(_ context.Context, diffs []model.FileDiff) (string, error) {
	files, err := ts.repo.headFiles()
	if err != nil {
		return "", err
	}

	for _, d := range diffs {
		path := filepath.ToSlash(d.Path)
		if d.Operation == model.DiffDeleted {
			delete(files, path)
			continue
		}
		files[path] = blobEntry{hash: plumbing.NewHash(d.NewHash), mode: filemode.Regular}
	}

	hash, err := writeNestedTree(ts.repo.repo.Storer, files)
	if err != nil {
		return "", err
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
