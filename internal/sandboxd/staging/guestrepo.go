package staging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// BuildForGuest is Build, then, unless guestRepo is false, a repository of
// the guest's own: the staged files as one base commit, with none of the
// host's history. The repository is made with go-git, in process: no git
// binary runs, so no host git configuration, hook, template or environment
// variable (GIT_DIR, GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY) can reach it, and
// it cannot write to any repository but the staged tree's own. The base
// commit has a fixed identity and a fixed date, so it is the same commit
// for the same files. The private .mgit store is excluded from it.
// An error is returned, never dropped; --no-guest-repo is the way to launch
// without the repository. Refs: FEAT-6.114.1, SEC-03
func BuildForGuest(worktreePath, privateStorePath, stagingDir string, guestRepo bool) error {
	if err := Build(worktreePath, privateStorePath, stagingDir); err != nil {
		return err
	}
	if !guestRepo {
		return nil
	}
	if err := initGuestRepo(stagingDir); err != nil {
		return fmt.Errorf("guest repository (launch with --no-guest-repo to skip it): %w", err)
	}
	return nil
}

// guestBaseCommitTime is the base commit's date: fixed, so the commit does
// not change from one launch to the next.
var guestBaseCommitTime = time.Unix(0, 0).UTC()

func initGuestRepo(dir string) error {
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	// the private store is the guest's mgit store, not part of its files
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(exclude, []byte(GuestStoreName+"/\n"), 0o600); err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	// go-git does not read .git/info/exclude when it adds, so the private
	// store is left out by naming what to add: every top-level entry but the
	// repository itself and the store.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// A path named to Add is added even when the project's ignore rules hide
	// it (go-git applies them only while it walks a directory), so a
	// top-level file such as debug.log is checked here against the same rules.
	// The file stays delivered; it is only not in the base commit.
	patterns, err := gitignore.ReadPatterns(wt.Filesystem, nil)
	if err != nil {
		return fmt.Errorf("read ignore rules: %w", err)
	}
	ignored := gitignore.NewMatcher(patterns)
	for _, e := range entries {
		if e.Name() == ".git" || e.Name() == GuestStoreName {
			continue
		}
		if ignored.Match([]string{e.Name()}, e.IsDir()) {
			continue
		}
		if err := wt.AddWithOptions(&git.AddOptions{Path: e.Name()}); err != nil {
			return fmt.Errorf("add %s: %w", e.Name(), err)
		}
	}
	sig := &object.Signature{Name: "mgit", Email: "mgit@localhost", When: guestBaseCommitTime}
	if _, err := wt.Commit("base: the files this sandbox was given", &git.CommitOptions{
		Author: sig, Committer: sig, AllowEmptyCommits: true,
	}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
