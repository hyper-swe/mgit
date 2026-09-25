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

// An environment value in a workflow is NOT shell-expanded: `FOO: ~/x` hands
// the step the literal string "~/x", which a script then treats as a relative
// directory named "~". The libkrunfw kernel-tarball cache (MGIT-163) did
// exactly that: build-libkrun.sh stored the tarball under ./~ in the
// workspace while actions/cache saved the real, empty ~/.cache/mgit-libkrun,
// so the cache never restored and every run refetched ~141 MB (measured on
// main's e2e run 35861088899: "Cache not found" … "stored … for later runs"
// … "Path Validation Error … no cache is being saved"). An action's own
// input (`path: ~/…`, lower case) is expanded by the action and is fine.
// A leading $VAR is exactly as literal and is caught the same way
// (literalEnvValues, held to fixtures in workflow_env_literal_test.go); the
// name stays because MGIT-238 cites it. Refs: MGIT-238, MGIT-247
func TestWorkflows_EnvValuesNeverStartWithATilde(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		for _, name := range literalEnvValues(readRepoFile(t, rel)) {
			t.Errorf("%s sets %s to a value starting with ~ or $, which the step receives literally; "+
				"expand it in the run step instead", rel, name)
		}
	}
}

// literalEnvEntry matches an env-style entry whose value starts, after an
// optional quote, with ~ or with a $ that does not open a ${{ }} expression.
var literalEnvEntry = regexp.MustCompile(`(?m)^\s+([A-Z][A-Z0-9_]*):\s*["']?(?:~|\$(?:[^{]|\{[^{]))`)

// literalEnvValues names every env-style entry in a workflow whose value the
// step would receive literally: a leading ~ or $VAR. The runner expands
// ${{ }} before the step starts; a shell would expand the others, and no
// shell reads an env: value. Refs: MGIT-247, MGIT-238
func literalEnvValues(workflow string) []string {
	matches := literalEnvEntry.FindAllStringSubmatch(workflow, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}
