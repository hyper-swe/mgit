package packaging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PINNED ACTIONS GET AN UPDATE PATH (MGIT-252). Every third-party action in
// the workflows is pinned to a full commit SHA with its release in a
// trailing comment (MGIT-246). A pin with no update path freezes the action,
// and its security fixes stop arriving. Dependabot's github-actions
// ecosystem reads the "# vX.Y.Z" comments and opens a pull request per new
// upstream release, moving the SHA and the comment together. This pins that
// the ecosystem is configured for the workflows directory on a weekly
// schedule. Refs: MGIT-252, MGIT-246
func TestDependabot_UpdatesThePinnedActionsWeekly(t *testing.T) {
	cfg := readRepoFile(t, ".github/dependabot.yml")
	assert.Contains(t, cfg, "version: 2", "dependabot.yml uses the version 2 schema")

	block := dependabotUpdate(cfg, "github-actions")
	require.NotEmpty(t, block, "dependabot.yml has an update for the github-actions ecosystem")
	assert.Contains(t, block, `directory: "/"`, "the ecosystem covers the repository's workflows")
	assert.Contains(t, block, `interval: "weekly"`, "the pins are checked weekly")
}

// dependabotUpdate returns the text of the updates entry whose
// package-ecosystem is ecosystem, from its "- package-ecosystem" line up to
// the next entry, or "" when there is none.
func dependabotUpdate(cfg, ecosystem string) string {
	entries := strings.Split(cfg, "\n  - ")
	for _, e := range entries[1:] {
		if strings.HasPrefix(e, `package-ecosystem: "`+ecosystem+`"`) {
			return e
		}
	}
	return ""
}
