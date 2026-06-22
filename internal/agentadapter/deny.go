package agentadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// denySettingsFiles are the Claude Code settings files, in no particular
// order, whose permissions.deny rules the adapter honors. Refs: MGIT-11.11.1
var denySettingsFiles = []string{
	filepath.Join(".claude", "settings.json"),
	filepath.Join(".claude", "settings.local.json"),
}

// LoadDenyRules gathers permissions.deny rules from the user settings
// (<home>/.claude/settings.json) and the worktree's project + project-local
// settings (<cwd>/.claude/settings*.json). Missing or malformed files are
// skipped — this is a best-effort safety augmentation, and a parse failure
// must not crash the hook (which would itself defeat fail-closed routing).
// Refs: MGIT-11.11.1
func LoadDenyRules(cwd, home string) []string {
	rules := make([]string, 0, len(denySettingsFiles)+1)
	rules = append(rules, denyRulesFrom(filepath.Join(home, ".claude", "settings.json"))...)
	for _, rel := range denySettingsFiles {
		rules = append(rules, denyRulesFrom(filepath.Join(cwd, rel))...)
	}
	return rules
}

// denyRulesFrom reads permissions.deny from one settings file, or returns
// nil if the file is absent or unparseable.
func denyRulesFrom(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // settings file under the user-owned cwd/home, read best-effort
	if err != nil {
		return nil
	}
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil
	}
	return doc.Permissions.Deny
}

// CommandDenied reports whether command matches any deny rule. The matcher
// is deliberately CONSERVATIVE: it errs toward reporting a match so a
// denied command is deferred to the user rather than auto-approved. It is
// not a full reimplementation of Claude Code's permission grammar — the
// authoritative deny enforcement remains Claude's engine; this check only
// prevents the mgit rewrite from laundering a denied command past it.
//
// Recognized forms: a bare "*" or "Bash" denies everything; "Bash(<spec>)"
// matches when the command shares <spec>'s literal prefix (a trailing "*"
// or ":*" marks an explicit prefix rule; otherwise an exact-or-prefix
// match on the rule text). Non-Bash rules are ignored. Refs: MGIT-11.11.1
func CommandDenied(command string, rules []string) bool {
	cmd := strings.TrimSpace(command)
	for _, rule := range rules {
		if denyRuleMatches(rule, cmd) {
			return true
		}
	}
	return false
}

// denyRuleMatches evaluates one deny rule against a normalized command.
func denyRuleMatches(rule, cmd string) bool {
	rule = strings.TrimSpace(rule)
	if rule == "*" || rule == "Bash" {
		return true
	}
	spec, ok := bashRuleSpec(rule)
	if !ok {
		return false // non-Bash rule — not our concern
	}
	spec = strings.TrimSpace(spec)
	spec = strings.TrimSuffix(spec, ":*")
	spec = strings.TrimSuffix(spec, "*")
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true // Bash() / Bash(*) — deny all
	}
	return cmd == spec || strings.HasPrefix(cmd, spec)
}

// bashRuleSpec extracts <spec> from a "Bash(<spec>)" rule, reporting
// whether the rule is a Bash rule at all.
func bashRuleSpec(rule string) (string, bool) {
	if !strings.HasPrefix(rule, "Bash(") || !strings.HasSuffix(rule, ")") {
		return "", false
	}
	return rule[len("Bash(") : len(rule)-1], true
}
