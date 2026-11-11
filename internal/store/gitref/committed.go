package gitref

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
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
	storage, err := NewReadOnlyStorage(osfs.New(commonDir(gitDir)))
	if err != nil {
		return nil, fmt.Errorf("%w: read git pack inventory: %w", ErrUnsupportedGitState, err)
	}
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

// CommittedFile is one file of git's committed tree with its content.
type CommittedFile struct {
	Path    string
	Mode    filemode.FileMode
	Content []byte
}

// CommittedFiles returns, READ-ONLY, every file git has committed at the
// project's local HEAD with its mode and content, and the HEAD commit id.
// Errors mirror CommittedBlobs. Refs: MGIT-283, ADR-008 §2,§6
func CommittedFiles(projectRoot string) ([]CommittedFile, string, error) {
	files, _, head, err := readCommittedFiles(projectRoot, false)
	return files, head, err
}

// CommittedSnapshot is git's committed tree as a new task's fork-base takes
// it: every committed file except the contents of a store nested in the
// project, and the folders that were left out.
type CommittedSnapshot struct {
	// Files are the committed files that belong to the project, with content.
	Files []CommittedFile
	// Skipped names, sorted and once each, every nested `.mgit` folder whose
	// committed contents were left out, as a project-relative path.
	Skipped []string
	// Head is the git HEAD commit id.
	Head string
}

// CommittedSnapshotFiles reads git's committed tree the way a new task's
// fork-base is built from it (MGIT-283). It is CommittedFiles with one
// narrowing: a `.mgit` directory below the project root is another store's
// data, not this project's content, so its files are neither read nor
// returned, and the folder is named in Skipped. Everything beside it stays.
// The root `.mgit`, a nested `.git`, a name that merely starts with `.mgit`,
// and a committed file or link that is itself named `.mgit` are not touched
// by this rule. Refs: MGIT-283, ADR-008 §2,§6
func CommittedSnapshotFiles(projectRoot string) (CommittedSnapshot, error) {
	files, skipped, head, err := readCommittedFiles(projectRoot, true)
	if err != nil {
		return CommittedSnapshot{}, err
	}
	return CommittedSnapshot{Files: files, Skipped: skipped, Head: head}, nil
}

// readCommittedFiles lists the tree's files with content. With skipNested, the
// files under a nested `.mgit` directory are dropped before their content is
// read.
func readCommittedFiles(projectRoot string, skipNested bool) (files []CommittedFile, skipped []string, head string, err error) {
	gitDir, err := resolveGitDir(projectRoot)
	if err != nil {
		return nil, nil, "", err
	}
	if err := assertSupportedState(gitDir); err != nil {
		return nil, nil, "", err
	}
	local, err := resolveHead(gitDir)
	if err != nil {
		return nil, nil, "", err
	}
	tree, err := headTree(gitDir, local.HeadCommit)
	if err != nil {
		return nil, nil, "", err
	}
	seen := map[string]bool{}
	if err := tree.Files().ForEach(func(f *object.File) error {
		if skipNested {
			if folder, ok := nestedMgitFolder(f.Name); ok {
				if !seen[folder] {
					seen[folder] = true
					skipped = append(skipped, folder)
				}
				return nil
			}
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		files = append(files, CommittedFile{Path: f.Name, Mode: f.Mode, Content: []byte(content)})
		return nil
	}); err != nil {
		return nil, nil, "", fmt.Errorf("%w: read git HEAD tree: %w", ErrUnsupportedGitState, err)
	}
	sort.Strings(skipped)
	return files, skipped, local.HeadCommit, nil
}

// nestedMgitFolder reports the nested `.mgit` directory a committed file path
// lies under: a `.mgit` component below the root that has something inside it.
// The component must be a whole directory name at depth one or more, so the
// root `.mgit`, `.mgitish` and a final component named `.mgit` do not match.
func nestedMgitFolder(path string) (string, bool) {
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == ".mgit" {
			return strings.Join(parts[:i+1], "/"), true
		}
	}
	return "", false
}
