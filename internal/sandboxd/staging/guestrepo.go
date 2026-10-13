package staging

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ErrNoGit is returned by BuildForGuest when the guest repository was asked
// for and the host has no git to make it with. The files were delivered; the
// guest simply has no repository. Callers carry on. Refs: FEAT-6.114.1
var ErrNoGit = errors.New("git is not installed on the host, so the guest was given no repository")

// BuildForGuest is Build, then, unless guestRepo is false, a repository of
// the guest's own: the staged files as one base commit, with none of the
// host's history. The base commit is made by git on the host, from the
// STAGED tree (already free of the host's .git), under a fixed identity,
// with the host's git configuration, templates and hooks out of the way.
// The private .mgit store is excluded from it. Refs: FEAT-6.114.1, SEC-03
func BuildForGuest(worktreePath, privateStorePath, stagingDir string, guestRepo bool) error {
	if err := Build(worktreePath, privateStorePath, stagingDir); err != nil {
		return err
	}
	if !guestRepo {
		return nil
	}
	return initGuestRepo(stagingDir)
}

func initGuestRepo(dir string) error {
	git, err := exec.LookPath("git")
	if err != nil {
		return ErrNoGit
	}
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_COUNT=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=mgit", "GIT_AUTHOR_EMAIL=mgit@localhost",
		"GIT_COMMITTER_NAME=mgit", "GIT_COMMITTER_EMAIL=mgit@localhost")
	run := func(args ...string) error {
		cmd := exec.Command(git, append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git, manager-owned staging dir
		cmd.Env = env
		if out, rerr := cmd.CombinedOutput(); rerr != nil {
			return fmt.Errorf("guest repository: git %s: %w: %s", args[0], rerr, out)
		}
		return nil
	}
	if err := run("init", "-q", "--template=", "-b", "main"); err != nil {
		return err
	}
	// the private store is the guest's mgit store, not part of its files
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		return fmt.Errorf("guest repository: %w", err)
	}
	if err := os.WriteFile(exclude, []byte(GuestStoreName+"/\n"), 0o600); err != nil {
		return fmt.Errorf("guest repository: %w", err)
	}
	if err := run("add", "-A"); err != nil {
		return err
	}
	return run("-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"commit", "-q", "--allow-empty", "--no-verify", "-m", "base: the files this sandbox was given")
}
