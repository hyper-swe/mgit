package packaging

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub refuses a workflow whose mapping repeats a key, and it says so only
// as a failed run "likely failed because of a workflow file issue" on the next
// push, never on the pull request. A YAML loader that keeps the last value
// (PyYAML does) reads the same file without a word. A release.yml step once
// carried two `with:` keys, so every branch push failed that way and the tag
// push that cuts the release would have too. Refs: MGIT-259
func TestWorkflows_NoMappingRepeatsAKey(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		assert.Empty(t, duplicateMappingKeys(readRepoFile(t, rel)), "%s repeats a key in one mapping", rel)
	}
}

// The checker is held to fixtures so its silence on the real workflows means
// something: it must name a repeated key, and must not confuse the same key in
// sibling steps, nested mappings or a block scalar's text with a repeat.
func TestDuplicateMappingKeys_Fixtures(t *testing.T) {
	tests := []struct {
		name string
		yml  string
		want []string
	}{
		{"repeated_with_in_one_step", "steps:\n  - uses: a\n    with:\n      name: x\n    with:\n      name: x\n", []string{"line 5: with"}},
		{"repeated_top_level_key", "on: push\nname: a\nname: b\n", []string{"line 3: name"}},
		{"same_key_in_sibling_steps", "steps:\n  - uses: a\n    with:\n      name: x\n  - uses: b\n    with:\n      name: y\n", nil},
		{"same_key_in_nested_mappings", "jobs:\n  a:\n    name: x\n  b:\n    name: y\n", nil},
		{"keys_inside_a_block_scalar", "run: |\n  name: x\n  name: y\nnext: 1\n", nil},
		{"comments_are_not_keys", "# name: a\nname: b\n  # name: c\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, duplicateMappingKeys(tt.yml))
		})
	}
}

// keyLine matches a block-mapping entry, optionally the first entry of a
// sequence item: indent, optional "- ", then a plain or quoted key and a colon.
var keyLine = regexp.MustCompile(`^( *)(- +)?("[^"]*"|'[^']*'|[A-Za-z0-9_.\-]+):(?: +(.*))?$`)

// blockScalar matches a value that opens a literal or folded block scalar.
var blockScalar = regexp.MustCompile(`^[|>][-+0-9]*(\s+#.*)?$`)

// mappingFrame is one open block mapping: the column its keys start at and
// the keys seen so far.
type mappingFrame struct {
	col  int
	keys map[string]bool
}

// duplicateMappingKeys names every key that repeats within one block mapping
// of a workflow, as "line N: key". It reads only the block style these
// workflows use; flow mappings and multi-line plain scalars are not entries.
func duplicateMappingKeys(yml string) []string {
	var found []string
	var stack []mappingFrame
	scalarCol := -1
	for i, line := range strings.Split(yml, "\n") {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if scalarCol >= 0 {
			if trimmed == "" || indent > scalarCol {
				continue
			}
			scalarCol = -1
		}
		m := keyLine.FindStringSubmatch(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || m == nil {
			continue
		}
		col := len(m[1]) + len(m[2])
		for len(stack) > 0 && (stack[len(stack)-1].col > col || (m[2] != "" && stack[len(stack)-1].col == col)) {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 || stack[len(stack)-1].col != col {
			stack = append(stack, mappingFrame{col: col, keys: map[string]bool{}})
		}
		top := stack[len(stack)-1]
		if top.keys[m[3]] {
			found = append(found, "line "+strconv.Itoa(i+1)+": "+m[3])
		}
		top.keys[m[3]] = true
		if blockScalar.MatchString(strings.TrimSpace(m[4])) {
			scalarCol = col
		}
	}
	return found
}
