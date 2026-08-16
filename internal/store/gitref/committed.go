package gitref

import (
	"fmt"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// CommittedBlobs returns, READ-ONLY, the file set git has COMMITTED at the
// project's current local HEAD: project-relative path -> git blob id (hex
// SHA-1). It is the authority for "what the user's git has committed", which
// under ADR-008 §3 (as amended by MGIT-123) is the ONLY content a read verb's
// auto-resync may absorb into the `.mgit` base — uncommitted working-tree
// content must stay uncommitted and visible so the user can attribute it to a
// task.
//
// It opens the git object store through go-git with a NIL worktree, so the
// repository is used for object reads alone and `.git` is never mutated
// (ADR-008 §6, MGIT-14). Objects and packs are read from the COMMON dir, so a
// linked git worktree (`.git`-as-file pointer) resolves to the shared object
// store rather than the per-worktree directory. Errors mirror ReadLocalState:
// ErrNoGit when there is no git at all, ErrDetachedOrUnborn when HEAD has no
// commit, ErrUnsupportedGitState when the objects cannot be read safely.
//
// Refs: MGIT-123, MGIT-35, ADR-008 §3,§6
func CommittedBlobs(projectRoot string) (map[string]string, error) {
	gitDir, err := resolveGitDir(projectRoot)
	if err != nil {
		return nil, err
	}
	if err := assertSupportedState(gitDir); err != nil {
		return nil, err
	}
	local, err := resolveHead(gitDir)
	if err != nil {
		return nil, err
	}
	tree, err := headTree(gitDir, local.HeadCommit)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]string)
	// Files() walks the tree recursively and yields full project-relative
	// paths; gitlink (submodule) entries are skipped, which is correct — their
	// content is not this repository's committed content.
	if err := tree.Files().ForEach(func(f *object.File) error {
		blobs[f.Name] = f.Hash.String()
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%w: walk git HEAD tree: %w", ErrUnsupportedGitState, err)
	}
	return blobs, nil
}

// headTree opens the project's git object store read-only and returns the tree
// of the given commit. Refs: MGIT-123, ADR-008 §6
func headTree(gitDir, headCommit string) (*object.Tree, error) {
	storage := filesystem.NewStorage(osfs.New(commonDir(gitDir)), cache.NewObjectLRUDefault())
	// A nil worktree makes this an object-read-only handle: go-git has no
	// filesystem to write into, so `.git` cannot be mutated through it.
	repo, err := gogit.Open(storage, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: open git object store: %w", ErrUnsupportedGitState, err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(headCommit))
	if err != nil {
		return nil, fmt.Errorf("%w: read git HEAD commit %s: %w", ErrUnsupportedGitState, headCommit, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("%w: read git HEAD tree: %w", ErrUnsupportedGitState, err)
	}
	return tree, nil
}
