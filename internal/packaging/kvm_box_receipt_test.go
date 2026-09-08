package packaging

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const kvmBoxReceiptWorkflow = ".github/workflows/kvm-box-receipt.yml"

// The KVM box is a self-hosted runner for a PUBLIC repository. A workflow
// that targets it must never be reachable from a fork's pull request, so the
// receipt workflow carries exactly one trigger: workflow_dispatch. Refs: MGIT-202
func TestKVMBoxReceipt_IsDispatchOnly(t *testing.T) {
	cfg := readRepoFile(t, kvmBoxReceiptWorkflow)
	assert.Equal(t, []string{"workflow_dispatch"}, workflowTriggers(cfg),
		"%s: the only trigger is workflow_dispatch — a pull_request or push trigger would let a fork's code run on the box", kvmBoxReceiptWorkflow)
}

// The runner label is read from a repository variable so the workflow file
// names no machine; the box is referred to by its label alone. Refs: MGIT-202
func TestKVMBoxReceipt_RunnerLabelComesFromTheRepositoryVariable(t *testing.T) {
	cfg := readRepoFile(t, kvmBoxReceiptWorkflow)
	runsOn := ""
	for _, line := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "runs-on:") {
			runsOn = strings.TrimSpace(line)
		}
	}
	require.NotEmpty(t, runsOn, "%s: no runs-on line", kvmBoxReceiptWorkflow)
	assert.Contains(t, runsOn, "vars.KVM_RUNNER_LABEL", "the label comes from the repository variable, not a literal")
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

// workflowTriggers returns the keys nested directly under the top-level
// `on:` block, in file order.
func workflowTriggers(cfg string) []string {
	var triggers []string
	inOn := false
	for _, line := range strings.Split(cfg, "\n") {
		switch {
		case strings.TrimRight(line, " ") == "on:":
			inOn = true
			continue
		case inOn && len(line) > 0 && line[0] != ' ' && line[0] != '#':
			return triggers
		case inOn && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && !strings.HasPrefix(strings.TrimSpace(line), "#"):
			triggers = append(triggers, strings.TrimSuffix(strings.TrimSpace(line), ":"))
		}
	}
	return triggers
}
