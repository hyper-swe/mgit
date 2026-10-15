package git_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/agentadapter"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

const (
	block   = "<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nscaffolding\n<!-- END mgit-sandbox -->\n"
	hashBlk = "# BEGIN mgit-sandbox (generated; do not edit)\nexport X=1\n# END mgit-sandbox\n"
)

func TestStripGeneratedBlock(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"block appended after user text", "# P\n\nrule\n\n" + block, "# P\n\nrule\n"},
		{"block in the middle keeps both sides", "above\n" + block + "below\n", "above\nbelow\n"},
		{"hash-comment syntax", "user=1\n\n" + hashBlk, "user=1\n"},
		{"no block is unchanged", "# P\n\nrule\n", "# P\n\nrule\n"},
		{"begin without end is never half-removed", "x\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nrest\n", "x\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nrest\n"},
		{"only the block leaves nothing", block, ""},
		{"empty", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(gitstore.StripGeneratedBlock([]byte(tt.in))))
		})
	}
}

// The markers this package strips are the ones agentadapter writes; if either
// side changes a phrase the committed file would silently keep the block.
// Refs: MGIT-126
func TestStripGeneratedBlock_RemovesWhatAgentadapterWrites(t *testing.T) {
	section := agentadapter.RenderClaudeMdSection(agentadapter.SandboxEnv{})
	got := gitstore.StripGeneratedBlock([]byte("# P\n\nrule\n\n" + section + "\n"))
	assert.Equal(t, "# P\n\nrule\n", string(got), "the rendered section is removed whole")
}
