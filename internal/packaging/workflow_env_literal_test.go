package packaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A workflow env: value is not shell-expanded: the step receives it as
// written. MGIT_LIBKRUN_CACHE: ~/.cache/… reached its step as a literal ~ and
// the cache never restored (MGIT-238). A value starting with $HOME or any
// $VAR is literal the same way. The scan is held to fixtures of every shape,
// because the workflows on main contain no such value and so cannot show it
// catches one. Refs: MGIT-247, MGIT-238
func TestLiteralEnvValues_NameEveryPrefixOnlyAShellWouldExpand(t *testing.T) {
	fixture := `jobs:
  j:
    env:
      TILDE: ~/.cache/x
      HOME_VAR: $HOME/.cache/x
      QUOTED: "$RUNNER_TEMP/y"
      SINGLE: '~/z'
      EXPR: ${{ github.workspace }}/x
      QUOTED_EXPR: "${{ runner.temp }}"
      PLAIN: /opt/x
      MIDDLE: /opt/$HOME
`
	assert.ElementsMatch(t, []string{"TILDE", "HOME_VAR", "QUOTED", "SINGLE"}, literalEnvValues(fixture),
		"a leading ~ or $ is literal to the step; ${{ }} is expanded by the runner and is fine")
}
