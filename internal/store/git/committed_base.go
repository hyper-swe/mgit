package git

import (
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// SnapshotFile is one file of an exact tree snapshot: its project-relative
// path, git file mode and content. Refs: MGIT-283
type SnapshotFile struct {
	Path    string
	Mode    filemode.FileMode
	Content []byte
}

// UncommittedAgainst lists, sorted, every path in this checkout whose state
// git has not committed: a file whose content differs from git's committed
// blob, a file git does not track, and a committed file missing from disk.
// Ignored paths are not listed. committed is git's committed set, path to
// blob id (gitref.CommittedBlobs). Refs: MGIT-283
func (r *Repository) UncommittedAgainst(committed map[string]string) ([]string, error) {
	paths, err := r.listWorkingFiles()
	if err != nil {
		return nil, fmt.Errorf("list working files: %w", err)
	}
	matching, err := r.PathsMatchingCommitted(committed, paths)
	if err != nil {
		return nil, err
	}
	same := make(map[string]bool, len(matching))
	for _, p := range matching {
		same[p] = true
	}
	onDisk := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		onDisk[p] = true
		if !same[p] {
			out = append(out, p)
		}
	}
	for p := range committed {
		if !onDisk[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// CreateDetachedCommit writes a commit whose tree is EXACTLY files, with the
// given parent, and moves no ref. It is the fork-base of a new task built from
// git's committed tree (MGIT-283): the task branch is created at it, while the
// main checkout's own branch, and every other branch, is left where it was.
// Refs: MGIT-283, ADR-008 §2
func (cs *CommitStore) CreateDetachedCommit(c *model.Commit, parent string, files []SnapshotFile) (string, error) {
	st := cs.repo.repo.Storer
	entries := make(map[string]blobEntry, len(files))
	for _, f := range files {
		if err := validateRelPath(f.Path); err != nil {
			return "", fmt.Errorf("snapshot %s: %w", f.Path, err)
		}
		h, err := writeBlob(st, f.Content)
		if err != nil {
			return "", err
		}
		entries[f.Path] = blobEntry{hash: h, mode: f.Mode}
	}
	tree, err := writeNestedTree(st, entries)
	if err != nil {
		return "", err
	}
	c.CreatedAt = cs.repo.Now()
	c.ParentID = parent
	c.TreeHash = tree.String()
	var parents []plumbing.Hash
	if parent != "" {
		parents = []plumbing.Hash{plumbing.NewHash(parent)}
	}
	hash, err := writeCommit(st, commitParams{
		tree: tree, parents: parents, message: c.Message,
		authorAt: object.Signature{Name: c.AgentID, Email: c.AgentID + "@mgit", When: c.CreatedAt},
	})
	if err != nil {
		return "", fmt.Errorf("create detached commit: %w", err)
	}
	c.CommitID = hash.String()
	c.ContentHash = c.ComputeContentHash()
	return c.CommitID, nil
}
