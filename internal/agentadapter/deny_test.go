package agentadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommandDenied_Rules verifies the conservative deny matcher against
// the Bash(...) rule forms Claude Code uses. Refs: MGIT-11.11.1
func TestCommandDenied_Rules(t *testing.T) {
	tests := []struct {
		name    string
		command string
		rules   []string
		want    bool
	}{
		{"prefix_rule_matches", "rm -rf /", []string{"Bash(rm:*)"}, true},
		{"prefix_rule_no_match", "npm test", []string{"Bash(rm:*)"}, false},
		{"exact_rule_matches", "make deploy", []string{"Bash(make deploy)"}, true},
		{"exact_rule_no_match", "make build", []string{"Bash(make deploy)"}, false},
		{"star_suffix_prefix", "curl http://x", []string{"Bash(curl*)"}, true},
		{"blanket_bash_deny", "anything", []string{"Bash"}, true},
		{"wildcard_deny", "anything", []string{"*"}, true},
		{"non_bash_rule_ignored", "rm -rf /", []string{"Read(*)"}, false},
		{"empty_rules", "rm -rf /", nil, false},
		{"leading_whitespace_command", "   rm -rf /", []string{"Bash(rm:*)"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CommandDenied(tt.command, tt.rules))
		})
	}
}

// TestLoadDenyRules_MergesUserAndProject verifies deny rules are gathered
// from the user, project, and project-local settings files (missing files
// are skipped). Refs: MGIT-11.11.1
func TestLoadDenyRules_MergesUserAndProject(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	writeSettings(t, filepath.Join(home, ".claude", "settings.json"),
		map[string]any{"permissions": map[string]any{"deny": []string{"Bash(rm:*)"}}})
	writeSettings(t, filepath.Join(cwd, ".claude", "settings.json"),
		map[string]any{"permissions": map[string]any{"deny": []string{"Bash(curl:*)"}}})
	writeSettings(t, filepath.Join(cwd, ".claude", "settings.local.json"),
		map[string]any{"permissions": map[string]any{"deny": []string{"Bash(wget:*)"}}})

	rules := LoadDenyRules(cwd, home)

	assert.Contains(t, rules, "Bash(rm:*)", "user deny")
	assert.Contains(t, rules, "Bash(curl:*)", "project deny")
	assert.Contains(t, rules, "Bash(wget:*)", "project-local deny")
}

// TestLoadDenyRules_NoFiles verifies absent settings files yield no rules
// and no error. Refs: MGIT-11.11.1
func TestLoadDenyRules_NoFiles(t *testing.T) {
	assert.Empty(t, LoadDenyRules(t.TempDir(), t.TempDir()))
}

// TestLoadDenyRules_SkipsMalformed verifies a corrupt settings file is
// skipped rather than crashing the hook (which would itself break
// fail-closed routing). Refs: MGIT-11.11.1
func TestLoadDenyRules_SkipsMalformed(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{bad json"), 0o600))
	assert.Empty(t, LoadDenyRules(cwd, t.TempDir()))
}

func writeSettings(t *testing.T, path string, v map[string]any) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}
