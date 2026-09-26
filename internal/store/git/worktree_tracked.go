package git

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// ignoreFilter decides which working paths the ignore rules hide, the way git
// does: an ignore rule applies to UNTRACKED paths only. A file git tracks
// stays tracked whatever a .gitignore says: one force-added under a `*.log`
// rule, or one committed inside a directory a later `build/` rule ignores.
// Hiding those from the walk kept them out of mgit's base, so no task
// worktree held them, a loop reading the worktree saw them as deleted, and
// `mgit status` never saw an edit to one (MGIT-269).
//
// "Tracked" is either store's answer: the path is in mgit's HEAD tree, or in
// the tree the project's git has committed at its HEAD (the import source, so
// a first import carries them). The tracked set is read lazily, on the first
// path a rule matches, so a walk no rule touches reads neither tree.
// Refs: MGIT-269, MGIT-32
type ignoreFilter struct {
	repo    *Repository
	matcher gitignore.Matcher
	loaded  bool
	files   map[string]bool // tracked file paths
	dirs    map[string]bool // every directory holding a tracked file
}

// hides reports whether the walk skips rel: true when an ignore rule matches
// it and nothing tracked is at or under it. Refs: MGIT-269, MGIT-32
func (f *ignoreFilter) hides(rel string, isDir bool) (bool, error) {
	if !f.matcher.Match(strings.Split(rel, "/"), isDir) {
		return false, nil
	}
	if err := f.load(); err != nil {
		return false, err
	}
	if isDir {
		return !f.dirs[rel], nil
	}
	return !f.files[rel], nil
}

// load reads the tracked set once.
func (f *ignoreFilter) load() error {
	if f.loaded {
		return nil
	}
	f.files, f.dirs = map[string]bool{}, map[string]bool{}
	head, err := f.repo.headFiles()
	if err != nil {
		return fmt.Errorf("read the paths mgit tracks: %w", err)
	}
	for p := range head {
		f.add(p)
	}
	committed, err := gitref.CommittedBlobs(f.repo.root)
	switch {
	case err == nil:
		for p := range committed {
			f.add(p)
		}
	case errors.Is(err, gitref.ErrNoGit), errors.Is(err, gitref.ErrDetachedOrUnborn):
		// No git, or a git with no commit, tracks nothing: a linked worktree,
		// a sandbox guest, a project mgit alone manages.
	default:
		return fmt.Errorf("read the paths git tracks: %w", err)
	}
	f.loaded = true
	return nil
}

// add records a tracked file and every directory above it.
func (f *ignoreFilter) add(p string) {
	f.files[p] = true
	for d := path.Dir(p); d != "." && d != "/" && !f.dirs[d]; d = path.Dir(d) {
		f.dirs[d] = true
	}
}
