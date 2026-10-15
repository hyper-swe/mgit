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
		{"text after the end marker on its line is kept byte for byte",
			"head\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\nx\n<!-- END mgit-sandbox --> tail\nmore\n", "head\n tail\nmore\n"},
		{"a marker that does not start its line is prose, not a marker",
			"keep <!-- BEGIN mgit-sandbox (generated; do not edit) -->\nx\n<!-- END mgit-sandbox -->\nrest\n",
			"keep <!-- BEGIN mgit-sandbox (generated; do not edit) -->\nx\n<!-- END mgit-sandbox -->\nrest\n"},
		{"prose that mentions both phrases is left intact",
			"Use BEGIN mgit-sandbox and END mgit-sandbox in prose.\nnext\n", "Use BEGIN mgit-sandbox and END mgit-sandbox in prose.\nnext\n"},
		{"prose mentioning the phrases beside a real block is kept",
			"see the BEGIN mgit-sandbox ... END mgit-sandbox markers\n\n<!-- BEGIN mgit-sandbox (a) -->\nx\n<!-- END mgit-sandbox -->\n", "see the BEGIN mgit-sandbox ... END mgit-sandbox markers\n"},
		{"a lone begin before a real span: the span opens at the LAST begin",
			"a\n<!-- BEGIN mgit-sandbox (lone) -->\nUSER TEXT\n<!-- BEGIN mgit-sandbox (real) -->\nz\n<!-- END mgit-sandbox -->\nw\n",
			"a\n<!-- BEGIN mgit-sandbox (lone) -->\nUSER TEXT\nw\n"},
		{"a stray end with no begin is left alone", "a\n<!-- END mgit-sandbox -->\nb\n", "a\n<!-- END mgit-sandbox -->\nb\n"},
		{"leading whitespace before the opener still anchors", "a\n  <!-- BEGIN mgit-sandbox (x) -->\nq\n  <!-- END mgit-sandbox -->\nb\n", "a\nb\n"},
		{"CRLF drops the separator line exactly as LF does",
			"# P\r\n\r\nrule\r\n\r\n<!-- BEGIN mgit-sandbox (generated; do not edit) -->\r\nx\r\n<!-- END mgit-sandbox -->\r\n", "# P\r\n\r\nrule\r\n"},
		{"any label: a complete mgit-sandbox span is generated",
			"user\n\n<!-- BEGIN mgit-sandbox (posture asserted by hyperswe: audit-only) -->\nx\n<!-- END mgit-sandbox -->\n", "user\n"},
		{"every complete span is removed",
			"a\n<!-- BEGIN mgit-sandbox (one) -->\nx\n<!-- END mgit-sandbox -->\nb\n<!-- BEGIN mgit-sandbox (two) -->\ny\n<!-- END mgit-sandbox -->\nc\n", "a\nb\nc\n"},
		{"an unterminated label is left alone",
			"x\n<!-- BEGIN mgit-sandbox (posture asserted by hyperswe: audit-only) -->\nrest\n", "x\n<!-- BEGIN mgit-sandbox (posture asserted by hyperswe: audit-only) -->\nrest\n"},
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
