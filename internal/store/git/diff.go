package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	fmtdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"

	"github.com/hyper-swe/mgit/internal/model"
)

// DiffStore computes file diffs between commits.
// Refs: FR-11, MGIT-2.2.5
type DiffStore struct {
	repo *Repository
}

// NewDiffStore creates a DiffStore backed by the given Repository.
func NewDiffStore(repo *Repository) *DiffStore {
	return &DiffStore{repo: repo}
}

// DiffCommits computes the file-level diff between two commits.
// Returns a list of FileDiffs describing what changed.
// Refs: FR-11
func (ds *DiffStore) DiffCommits(_ context.Context, fromHash, toHash string) ([]model.FileDiff, error) {
	goRepo := ds.repo.repo

	fromCommit, err := goRepo.CommitObject(hashFromString(fromHash))
	if err != nil {
		return nil, fmt.Errorf("%w: from commit %s", model.ErrCommitNotFound, fromHash)
	}
	toCommit, err := goRepo.CommitObject(hashFromString(toHash))
	if err != nil {
		return nil, fmt.Errorf("%w: to commit %s", model.ErrCommitNotFound, toHash)
	}

	fromTree, err := fromCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get from tree: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get to tree: %w", err)
	}

	changes, err := fromTree.Diff(toTree)
	if err != nil {
		return nil, fmt.Errorf("compute diff: %w", err)
	}

	diffs := make([]model.FileDiff, 0, len(changes))
	for _, change := range changes {
		fd := changeToFileDiff(change)
		diffs = append(diffs, fd)
	}

	return diffs, nil
}

// PatchBetween renders the changes between two commits as a standard git
// unified diff (the `diff --git ... @@ ... +/-content` text), via go-git's own
// patch encoder — so it is byte-for-byte git-apply/git-am compatible (correct
// /dev/null for adds, line numbers, and content), unlike a hand-rolled
// FileDiff render. fromHash may be empty to diff against the empty tree (every
// file an addition). This is the content body behind `mgit squash --to-git`,
// the mgit->git delivery bridge. Refs: FR-7, MGIT-33
func (ds *DiffStore) PatchBetween(ctx context.Context, fromHash, toHash string) (string, error) {
	// An empty "to" is never a legitimate empty tree here: it means the caller
	// has no commit to diff. Reject it rather than render a hunk-free patch
	// that `git apply` accepts and applies nothing (MGIT-112).
	if toHash == "" {
		return "", fmt.Errorf("%w: to commit is empty", model.ErrCommitNotFound)
	}
	toTree, err := ds.commitTree(toHash, "to")
	if err != nil {
		return "", err
	}
	fromTree, err := ds.commitTree(fromHash, "from")
	if err != nil {
		return "", err
	}
	return ds.patchBetweenTrees(ctx, fromTree, toTree)
}

// PatchFromCommitToTree renders the same unified diff as PatchBetween, but the
// "to" side is a raw TREE hash rather than a commit. That is what a read-only
// export needs: the net result of squashing a task exists as a tree before any
// squash commit is created, and `mgit export --format git` must never create
// one. Sharing patchBetweenTrees with PatchBetween is what makes the export and
// `mgit squash --to-git` byte-identical in their hunks by construction.
// fromCommit may be empty to diff against the empty tree (every file an
// addition). Refs: FR-7, MGIT-33, MGIT-112
func (ds *DiffStore) PatchFromCommitToTree(ctx context.Context, fromCommit, toTree string) (string, error) {
	if toTree == "" {
		return "", fmt.Errorf("patch to tree: empty target tree hash")
	}
	to, err := object.GetTree(ds.repo.repo.Storer, hashFromString(toTree))
	if err != nil {
		return "", fmt.Errorf("get to tree %s: %w", toTree, err)
	}
	from, err := ds.commitTree(fromCommit, "from")
	if err != nil {
		return "", err
	}
	return ds.patchBetweenTrees(ctx, from, to)
}

// commitTree resolves a commit hash to its tree. An empty hash yields a nil
// tree, which go-git's differ reads as the empty tree (every file an addition).
// side names the operand in error messages ("from" / "to").
func (ds *DiffStore) commitTree(hash, side string) (*object.Tree, error) {
	if hash == "" {
		return nil, nil //nolint:nilnil // a nil tree IS the empty tree for go-git's differ
	}
	commit, err := ds.repo.repo.CommitObject(hashFromString(hash))
	if err != nil {
		return nil, fmt.Errorf("%w: %s commit %s", model.ErrCommitNotFound, side, hash)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("get %s tree: %w", side, err)
	}
	return tree, nil
}

// patchBetweenTrees renders the unified diff between two trees via go-git's own
// patch encoder — the single renderer behind every mgit->git patch, so squash
// and export cannot drift apart. A nil tree is the empty tree. Refs: MGIT-33
func (ds *DiffStore) patchBetweenTrees(ctx context.Context, from, to *object.Tree) (string, error) {
	changes, err := object.DiffTreeContext(ctx, from, to)
	if err != nil {
		return "", fmt.Errorf("compute patch: %w", err)
	}
	patch, err := changes.PatchContext(ctx)
	if err != nil {
		return "", fmt.Errorf("render patch: %w", err)
	}
	return patch.String(), nil
}

// DiffStats returns aggregate statistics for a diff between two commits.
// Refs: FR-11
func (ds *DiffStore) DiffStats(ctx context.Context, fromHash, toHash string) (model.DiffStatistics, error) {
	diffs, err := ds.DiffCommits(ctx, fromHash, toHash)
	if err != nil {
		return model.DiffStatistics{}, err
	}

	var stats model.DiffStatistics
	for _, d := range diffs {
		s := d.Statistics()
		stats.LinesAdded += s.LinesAdded
		stats.LinesRemoved += s.LinesRemoved
	}
	return stats, nil
}

// changeToFileDiff converts a go-git tree change to a model.FileDiff.
func changeToFileDiff(change *object.Change) model.FileDiff {
	action, err := change.Action()
	if err != nil {
		return model.FileDiff{Path: change.To.Name, Operation: model.DiffModified}
	}

	fd := model.FileDiff{}

	// go-git's merkletrie.Action is `_ = iota; Insert; Delete; Modify`, i.e.
	// Insert=1, Delete=2, Modify=3 (0 is the unused blank). Switch on the NAMED
	// constants — the prior magic 0/1/2 mis-mapped every action (Insert hit the
	// Delete arm and read the empty From side -> empty-path "deleted" entries).
	// Refs: MGIT-33
	switch action {
	case merkletrie.Insert:
		fd.Path = change.To.Name
		fd.Operation = model.DiffAdded
		fd.NewHash = change.To.TreeEntry.Hash.String()
		fd.Mode = diffModeFromGit(change.To.TreeEntry.Mode)
	case merkletrie.Delete:
		fd.Path = change.From.Name
		fd.Operation = model.DiffDeleted
		fd.OldHash = change.From.TreeEntry.Hash.String()
		fd.Mode = diffModeFromGit(change.From.TreeEntry.Mode)
		fd.OldMode = diffModeFromGit(change.From.TreeEntry.Mode)
	case merkletrie.Modify:
		fd.Path = change.To.Name
		fd.Operation = model.DiffModified
		fd.OldHash = change.From.TreeEntry.Hash.String()
		fd.NewHash = change.To.TreeEntry.Hash.String()
		fd.Mode = diffModeFromGit(change.To.TreeEntry.Mode)
		fd.OldMode = diffModeFromGit(change.From.TreeEntry.Mode)
	}

	fd.Hunks = changeHunks(change)
	return fd
}

// diffModeFromGit is the inverse of gitModeFromDiff (tree.go): it records the
// git file mode on a FileDiff so a tree rebuilt from diffs (rollback,
// cherry-pick — MGIT-54) preserves the executable bit / symlink-ness instead
// of collapsing everything to a regular file. Refs: MGIT-54, MGIT-16
func diffModeFromGit(m filemode.FileMode) model.FileDiffMode {
	switch m {
	case filemode.Executable:
		return model.FileModeExecutable
	case filemode.Symlink:
		return model.FileModeSymlink
	default:
		return model.FileModeRegular
	}
}

// changeHunks renders a single display hunk (line content with +/-/space
// prefixes) for a tree change via go-git's own patch encoder, so `mgit diff`
// and `mgit show` show real content — not just file-level adds/deletes. It
// degrades gracefully: a binary file or any patch error yields no hunk (the
// file-level FileDiff still stands). Line numbers are display-oriented (start 1);
// the git-apply-correct export uses DiffStore.PatchBetween. Refs: FR-11, MGIT-33
func changeHunks(change *object.Change) []model.Hunk {
	patch, err := change.Patch()
	if err != nil {
		return nil
	}
	fps := patch.FilePatches()
	if len(fps) == 0 || fps[0].IsBinary() {
		return nil
	}
	var content strings.Builder
	var added, removed int
	for _, chunk := range fps[0].Chunks() {
		prefix := byte(' ')
		switch chunk.Type() {
		case fmtdiff.Add:
			prefix = '+'
		case fmtdiff.Delete:
			prefix = '-'
		}
		for _, ln := range strings.SplitAfter(chunk.Content(), "\n") {
			if ln == "" {
				continue
			}
			content.WriteByte(prefix)
			content.WriteString(ln)
			if !strings.HasSuffix(ln, "\n") {
				content.WriteByte('\n')
			}
			switch prefix {
			case '+':
				added++
			case '-':
				removed++
			}
		}
	}
	if content.Len() == 0 {
		return nil
	}
	return []model.Hunk{{LineStart: 1, LinesAdded: added, LinesRemoved: removed, Content: content.String()}}
}
