package git

import (
	"errors"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"

	"github.com/hyper-swe/mgit/internal/model"
)

// AuthorIdentity is the person an exported patch is authored by.
type AuthorIdentity struct {
	Name  string
	Email string
}

// ResolveAuthorIdentity finds the git identity of the person exporting a
// patch, with git's own precedence for each field separately:
// GIT_AUTHOR_NAME / GIT_AUTHOR_EMAIL, then author.name / author.email, then
// user.name / user.email, where the config is the project's own git config
// merged over the global one. It reads config through go-git and never runs
// git. projectRoot is the directory holding the project's .git; a project
// with no git repository uses the global config alone. A missing name or
// email is model.ErrNoPatchIdentity, naming the settings that would supply
// it. Refs: MGIT-237
func ResolveAuthorIdentity(projectRoot string, getenv func(string) string) (AuthorIdentity, error) {
	cfg, err := identityConfig(projectRoot)
	if err != nil {
		return AuthorIdentity{}, err
	}
	id := AuthorIdentity{
		Name:  firstSet(getenv("GIT_AUTHOR_NAME"), cfg.Author.Name, cfg.User.Name),
		Email: firstSet(getenv("GIT_AUTHOR_EMAIL"), cfg.Author.Email, cfg.User.Email),
	}
	if id.Name == "" || id.Email == "" {
		return AuthorIdentity{}, fmt.Errorf("%w: git am records a patch's From: line as the commit's author, "+
			"so set user.name and user.email (git config, in this project or globally) "+
			"or GIT_AUTHOR_NAME and GIT_AUTHOR_EMAIL", model.ErrNoPatchIdentity)
	}
	return id, nil
}

// identityConfig is the project's git config merged over the global one, or
// the global one alone when projectRoot holds no git repository.
func identityConfig(projectRoot string) (*config.Config, error) {
	if repo, err := gogit.PlainOpen(projectRoot); err == nil {
		cfg, cerr := repo.ConfigScoped(config.GlobalScope)
		if cerr != nil {
			return nil, fmt.Errorf("read the project's git config: %w", cerr)
		}
		return cfg, nil
	} else if !errors.Is(err, gogit.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("open the project's git repository: %w", err)
	}
	cfg, err := config.LoadConfig(config.GlobalScope)
	if err != nil {
		return nil, fmt.Errorf("read the global git config: %w", err)
	}
	return cfg, nil
}

// firstSet returns the first value that is not blank, trimmed.
func firstSet(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
