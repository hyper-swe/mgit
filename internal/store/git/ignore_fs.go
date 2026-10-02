package git

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// ignoreReadFS is the filesystem the project's .gitignore files are read
// through. It differs from a plain one in what a directory listing shows:
// anything the project walk would not enter (excludedNames, and a nested mgit
// root such as a linked worktree) is absent, and a name that disappears
// between the listing and its stat is absent too rather than an error.
//
// The rule reader (go-git's ReadPatterns) descends into every directory it is
// shown. Shown a sibling worktree's .mgit, it read a directory another `mgit
// work` was creating and removing temporary files in, and a path that vanished
// there failed the whole worktree add. The listing walk (listWorkingFiles)
// already prunes those directories; the rule reader now sees the same tree.
// Refs: MGIT-285, MGIT-120, MGIT-157
type ignoreReadFS struct {
	billy.Filesystem
	root    string
	readDir func(string) ([]os.DirEntry, error)
}

// newIgnoreReadFS returns the pruned view of the project at root.
func newIgnoreReadFS(root string) *ignoreReadFS {
	return &ignoreReadFS{Filesystem: osfs.New(root), root: root, readDir: os.ReadDir}
}

// ReadDir lists path (relative to the project root) without the directories
// the project walk never enters and without names that vanish mid-listing.
// A directory that vanishes or is unreadable is reported to the caller as
// before; only entries inside a listing that did succeed are filtered.
// Refs: MGIT-285
func (f *ignoreReadFS) ReadDir(path string) ([]os.FileInfo, error) {
	dir := filepath.Join(f.root, path)
	entries, err := f.readDir(dir)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		if excludedNames[e.Name()] {
			continue
		}
		if e.IsDir() && isNestedMgitRoot(filepath.Join(dir, e.Name())) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}
