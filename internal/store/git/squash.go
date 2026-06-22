package git

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// SquashCommitParams carries the inputs to build a task-isolated squash commit.
// TaskCommits are the task's micro-commit hashes in position (chronological)
// order; Branch is the dedicated task ref short name (e.g. "task/MGIT-1") the
// squash is placed on. Refs: FR-7, MGIT-22
type SquashCommitParams struct {
	Commit      *model.Commit
	TaskCommits []string
	Branch      string
}

// CreateSquashCommit consolidates a task's micro-commits into a single commit
// that captures ONLY that task's net file changes, parents it off the task's
// base (the parent of its first micro-commit), and places it on its own Branch
// ref. It NEVER advances HEAD or the integration branch and NEVER rewrites or
// removes the original micro-commits: per the append-only law (FR-12) the
// originals remain the audit trail and this commit is the task's clean,
// exportable deliverable. The task's net change is derived by diffing each
// micro-commit against its own parent (so an interleaved unrelated task's
// changes are excluded) and layering the result, last-write-wins, onto the
// base tree. Populates the commit's CommitID/ContentHash/CreatedAt/ParentID/
// TreeHash/Branch. Refs: FR-7, FR-12, MGIT-22
func (cs *CommitStore) CreateSquashCommit(_ context.Context, p SquashCommitParams) (string, error) {
	if len(p.TaskCommits) == 0 {
		return "", fmt.Errorf("squash: %w", model.ErrTaskNotFound)
	}
	if p.Branch == "" {
		return "", fmt.Errorf("squash: target branch must not be empty")
	}
	goRepo := cs.repo.repo

	hashes := make([]plumbing.Hash, len(p.TaskCommits))
	for i, h := range p.TaskCommits {
		hashes[i] = plumbing.NewHash(h)
	}

	// The base is the parent of the task's first micro-commit; the squash tree
	// starts from the base tree (empty when the task began at a root commit).
	baseHash, baseFiles, err := cs.squashBase(hashes[0])
	if err != nil {
		return "", err
	}

	// Layer each micro-commit's own delta onto the base, last-write-wins.
	net, err := cs.taskNetChanges(hashes)
	if err != nil {
		return "", err
	}
	for path, entry := range net {
		if entry == nil {
			delete(baseFiles, path)
			continue
		}
		baseFiles[path] = *entry
	}

	treeHash, err := writeNestedTree(goRepo.Storer, baseFiles)
	if err != nil {
		return "", err
	}

	c := p.Commit
	c.CreatedAt = cs.repo.Now()
	c.TreeHash = treeHash.String()
	var parents []plumbing.Hash
	if !baseHash.IsZero() {
		parents = []plumbing.Hash{baseHash}
		c.ParentID = baseHash.String()
	} else {
		c.ParentID = ""
	}

	commitHash, err := writeCommit(goRepo.Storer, commitParams{
		tree:    treeHash,
		parents: parents,
		message: c.Message,
		authorAt: object.Signature{
			Name:  c.AgentID,
			Email: c.AgentID + "@mgit",
			When:  c.CreatedAt,
		},
	})
	if err != nil {
		return "", fmt.Errorf("squash: create commit: %w", err)
	}
	c.CommitID = commitHash.String()
	c.ContentHash = c.ComputeContentHash()
	c.Branch = p.Branch

	// Point the task's own branch at the squash. SetReference creates the ref or
	// moves it (re-squash); the integration branch and HEAD are left untouched.
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.Branch), commitHash)
	if err := goRepo.Storer.SetReference(ref); err != nil {
		return "", fmt.Errorf("squash: set task branch %s: %w", p.Branch, err)
	}

	return c.CommitID, nil
}

// squashBase returns the base commit hash and its flattened tree for the task's
// first micro-commit: the parent of that commit, or the zero hash and an empty
// tree when the task began at a root (parentless) commit. ParentHashes[0] is
// safe here (and in taskNetChanges): task_commits only ever indexes
// single-parent commits — normal commits, squashes, and rollbacks (see the
// AddCommitToTask call sites); two-parent merge commits are never task-indexed,
// so a task's micro-commits never have a second parent to account for.
// Refs: MGIT-22
func (cs *CommitStore) squashBase(first plumbing.Hash) (plumbing.Hash, map[string]blobEntry, error) {
	obj, err := cs.repo.repo.CommitObject(first)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("squash: read first commit %s: %w", first, err)
	}
	if obj.NumParents() == 0 {
		return plumbing.ZeroHash, make(map[string]blobEntry), nil
	}
	baseHash := obj.ParentHashes[0]
	files, err := cs.commitTreeFiles(baseHash)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return baseHash, files, nil
}

// taskNetChanges computes the net file changes attributable to the given
// commits by diffing each against its OWN parent and applying the deltas in
// order (later commits win). A path maps to a non-nil entry when set/updated,
// or to nil when the task's last touch deleted it. Diffing against each
// commit's parent (rather than a single span) isolates the task's own changes
// even when unrelated commits are interleaved on the branch. Refs: MGIT-22
func (cs *CommitStore) taskNetChanges(commits []plumbing.Hash) (map[string]*blobEntry, error) {
	net := make(map[string]*blobEntry)
	for _, h := range commits {
		obj, err := cs.repo.repo.CommitObject(h)
		if err != nil {
			return nil, fmt.Errorf("squash: read commit %s: %w", h, err)
		}
		cur, err := cs.commitTreeFiles(h)
		if err != nil {
			return nil, err
		}
		var parent map[string]blobEntry
		if obj.NumParents() > 0 {
			parent, err = cs.commitTreeFiles(obj.ParentHashes[0])
			if err != nil {
				return nil, err
			}
		} else {
			parent = make(map[string]blobEntry)
		}
		applyTreeDelta(net, parent, cur)
	}
	return net, nil
}

// applyTreeDelta records, into net, the changes from parent to cur: a path
// added, or whose blob hash OR file mode changed, is set to its cur entry; a
// path present in parent but absent from cur is marked deleted (nil). The mode
// is compared as well as the hash so a mode-only change (chmod +x, or a
// regular file replaced by a symlink with identical bytes) is not dropped —
// mgit tracks the executable/symlink distinction faithfully (worktree_fs.go),
// so it must survive a squash. Refs: MGIT-22
func applyTreeDelta(net map[string]*blobEntry, parent, cur map[string]blobEntry) {
	for path, e := range cur {
		prev, existed := parent[path]
		if !existed || prev.hash != e.hash || prev.mode != e.mode {
			entry := e
			net[path] = &entry
		}
	}
	for path := range parent {
		if _, stillThere := cur[path]; !stillThere {
			net[path] = nil
		}
	}
}

// commitTreeFiles returns the flattened file set (path → blob+mode) of a
// commit's tree. Refs: MGIT-22
func (cs *CommitStore) commitTreeFiles(h plumbing.Hash) (map[string]blobEntry, error) {
	obj, err := cs.repo.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("squash: read commit %s: %w", h, err)
	}
	tree, err := obj.Tree()
	if err != nil {
		return nil, fmt.Errorf("squash: read tree for %s: %w", h, err)
	}
	return flattenTree(tree)
}
