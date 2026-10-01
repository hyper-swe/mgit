package git

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The exporter's identity follows git's precedence for each field on its
// own: GIT_AUTHOR_* over author.* over user.*, with the project's git config
// over the global one. Every source is isolated in a temp directory, so the
// machine's own git config cannot decide these. Refs: MGIT-237
func TestResolveAuthorIdentity_FollowsGitsPrecedencePerField(t *testing.T) {
	type src struct {
		env            map[string]string
		global         string // a ~/.gitconfig body
		project        string // the project's .git/config [user]/[author] body
		projectHasRepo bool
	}
	tests := []struct {
		name      string
		src       src
		wantName  string
		wantEmail string
	}{
		{"the environment alone", src{env: map[string]string{"GIT_AUTHOR_NAME": "Env Name", "GIT_AUTHOR_EMAIL": "env@example.com"}},
			"Env Name", "env@example.com"},
		{"the global config alone", src{global: "[user]\n\tname = Global Name\n\temail = global@example.com\n"},
			"Global Name", "global@example.com"},
		{"the project config over the global one", src{projectHasRepo: true,
			global:  "[user]\n\tname = Global Name\n\temail = global@example.com\n",
			project: "[user]\n\tname = Project Name\n\temail = project@example.com\n"},
			"Project Name", "project@example.com"},
		{"author.* over user.*", src{projectHasRepo: true,
			project: "[user]\n\tname = User Name\n\temail = user@example.com\n[author]\n\tname = Author Name\n\temail = author@example.com\n"},
			"Author Name", "author@example.com"},
		{"each field on its own: the name from the environment, the email from config", src{
			env:    map[string]string{"GIT_AUTHOR_NAME": "Env Name"},
			global: "[user]\n\tname = Global Name\n\temail = global@example.com\n"},
			"Env Name", "global@example.com"},
		{"a blank environment value is no value", src{
			env:    map[string]string{"GIT_AUTHOR_NAME": "  ", "GIT_AUTHOR_EMAIL": ""},
			global: "[user]\n\tname = Global Name\n\temail = global@example.com\n"},
			"Global Name", "global@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := isolateIdentitySources(t, tt.src.global)
			if tt.src.projectHasRepo {
				_, err := gogit.PlainInit(project, false)
				require.NoError(t, err)
				appendGitConfig(t, filepath.Join(project, ".git", "config"), tt.src.project)
			}
			getenv := func(k string) string { return tt.src.env[k] }

			id, err := ResolveAuthorIdentity(project, getenv)

			require.NoError(t, err)
			assert.Equal(t, tt.wantName, id.Name)
			assert.Equal(t, tt.wantEmail, id.Email)
		})
	}
}

// No name or no email anywhere is model.ErrNoPatchIdentity, naming the git
// settings that would supply it. Refs: MGIT-237
func TestResolveAuthorIdentity_Missing_NamesTheSettings(t *testing.T) {
	for name, global := range map[string]string{
		"nothing":        "",
		"no email":       "[user]\n\tname = Only A Name\n",
		"no name either": "[core]\n\tbare = false\n",
	} {
		t.Run(name, func(t *testing.T) {
			project := isolateIdentitySources(t, global)
			_, err := ResolveAuthorIdentity(project, func(string) string { return "" })
			require.ErrorIs(t, err, model.ErrNoPatchIdentity)
			assert.Contains(t, err.Error(), "user.name")
			assert.Contains(t, err.Error(), "user.email")
		})
	}
}

// isolateIdentitySources points the global git config at a fresh HOME that
// holds globalConfig (when non-empty) and returns an empty project directory.
func isolateIdentitySources(t *testing.T, globalConfig string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if globalConfig != "" {
		require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(globalConfig), 0o600))
	}
	return t.TempDir()
}

// appendGitConfig appends a config body to a git config file.
func appendGitConfig(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	_, err = f.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}
