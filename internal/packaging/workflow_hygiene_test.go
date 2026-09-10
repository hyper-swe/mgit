package packaging

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This repository is PUBLIC, and a fork's pull request must never be able to
// run on a machine of ours: no workflow under .github/workflows may target a
// self-hosted runner, by literal label or through a repository variable.
// The founder's ruling of 2026-09-10: the KVM legs run on hosted runners
// (e2e.yml applies the udev rule on ubuntu-latest and boots both guest
// kinds on every push), and the KVM box is for the billed repository's legs
// only. Refs: MGIT-202
func TestWorkflows_NeverTargetASelfHostedRunner(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	runsOn := regexp.MustCompile(`(?m)^\s*runs-on:\s*(.+)$`)
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		for _, m := range runsOn.FindAllStringSubmatch(readRepoFile(t, rel), -1) {
			target := m[1]
			assert.NotContains(t, target, "self-hosted", "%s targets a self-hosted runner: %s", rel, target)
			assert.NotContains(t, target, "vars.", "%s picks its runner from a repository variable, which is how a self-hosted label would reach a public workflow: %s", rel, target)
		}
	}
}

// The box's address is private. No workflow under .github/workflows may carry
// a literal IPv4 address; the box is addressed by runner label only. Refs: MGIT-202
func TestWorkflows_CarryNoLiteralAddress(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no workflows found under %s", root)
	quad := regexp.MustCompile(`\b[0-9]{1,3}(\.[0-9]{1,3}){3}\b`)
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		cfg := readRepoFile(t, rel)
		assert.Empty(t, quad.FindAllString(cfg, -1), "%s carries a literal address", rel)
	}
}
