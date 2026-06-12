package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRepoFile loads a documentation file from the repository root.
func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	data, err := os.ReadFile(path) //nolint:gosec // OK: test reads repo-tracked docs at paths built from string literals above

	require.NoError(t, err, "repo file %s must exist", path)
	return string(data)
}

// TestRTM_FR17_AllCriteriaHaveIDs verifies that REQUIREMENTS.md contains
// FR-17 and NFR-17 sections whose criteria all carry stable, unique,
// well-formed requirement IDs. Refs: FR-17, NFR-17, MGIT-11.1.1
func TestRTM_FR17_AllCriteriaHaveIDs(t *testing.T) {
	reqs := readRepoFile(t, "REQUIREMENTS.md")

	tests := []struct {
		name    string
		prefix  string
		idShape *regexp.Regexp
		minimum int
	}{
		{
			name:    "fr17_criteria",
			prefix:  "FR-17",
			idShape: regexp.MustCompile(`^FR-17\.\d+[a-z]?$`),
			minimum: 15,
		},
		{
			name:    "nfr17_criteria",
			prefix:  "NFR-17",
			idShape: regexp.MustCompile(`^NFR-17\.\d+[a-z]?$`),
			minimum: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			criteria := ParseRequirements(reqs, tt.prefix)
			require.GreaterOrEqual(t, len(criteria), tt.minimum,
				"%s must define at least %d numbered criteria", tt.prefix, tt.minimum)

			seen := make(map[string]bool, len(criteria))
			for _, c := range criteria {
				assert.Regexp(t, tt.idShape, c.ID, "criterion ID must be stable and well-formed")
				assert.False(t, seen[c.ID], "criterion ID %s must be unique", c.ID)
				assert.NotEmpty(t, c.Text, "criterion %s must have requirement text", c.ID)
				seen[c.ID] = true
			}
		})
	}
}

// TestRTM_FR17_MapsToADR005Goals verifies every numbered Goal in ADR-005
// maps to at least one FR-17/NFR-17 criterion via the traceability table
// in REQUIREMENTS.md, and that every mapped ID exists.
// Refs: FR-17, MGIT-11.1.1
func TestRTM_FR17_MapsToADR005Goals(t *testing.T) {
	reqs := readRepoFile(t, "REQUIREMENTS.md")
	adr := readRepoFile(t, "docs", "adr", "005-microvm-sandbox.md")

	goalCount := CountADRGoals(adr)
	require.GreaterOrEqual(t, goalCount, 8, "ADR-005 must declare its numbered goals")

	mappings := ParseGoalMappings(reqs)
	defined := make(map[string]bool)
	for _, c := range ParseRequirements(reqs, "FR-17") {
		defined[c.ID] = true
	}
	for _, c := range ParseRequirements(reqs, "NFR-17") {
		defined[c.ID] = true
	}

	for goal := 1; goal <= goalCount; goal++ {
		ids := mappings[goal]
		assert.NotEmpty(t, ids, "ADR-005 goal %d must map to >=1 requirement criterion", goal)
		for _, id := range ids {
			assert.True(t, defined[id],
				"goal %d maps to %s, which must be a defined criterion", goal, id)
		}
	}
}

// TestRTM_NFR17_PerfTargetsQuantified verifies the NFR-17 performance
// criteria are quantified (numbers with units), so none is unfalsifiable.
// Targets come from ADR-005 "Resource Budget". Refs: NFR-17, MGIT-11.1.1
func TestRTM_NFR17_PerfTargetsQuantified(t *testing.T) {
	reqs := readRepoFile(t, "REQUIREMENTS.md")
	criteria := ParseRequirements(reqs, "NFR-17")
	require.NotEmpty(t, criteria, "NFR-17 section must exist")

	texts := make([]string, len(criteria))
	for i, c := range criteria {
		texts[i] = c.Text
	}
	section := strings.Join(texts, "\n")

	targets := []struct {
		name    string
		pattern string
	}{
		{"warm_exec_overhead_50ms", `<\s*50\s*ms`},
		{"warm_start_200ms", `<\s*200\s*ms`},
		{"cold_boot_1s", `<\s*1\s*s`},
		{"idle_memory_100mb", `100\s*MB`},
		{"five_idle_sandboxes_500mb", `<\s*500\s*MB`},
	}
	for _, tt := range targets {
		t.Run(tt.name, func(t *testing.T) {
			assert.Regexp(t, tt.pattern, section,
				"NFR-17 must quantify target %s", tt.name)
		})
	}

	for _, c := range criteria {
		assert.Regexp(t, `\d`, c.Text,
			"criterion %s must contain a quantified target", c.ID)
	}
}

// TestParseRequirements_NoMatches_ReturnsEmpty covers the error path:
// a document without the requested prefix yields no criteria.
func TestParseRequirements_NoMatches_ReturnsEmpty(t *testing.T) {
	assert.Empty(t, ParseRequirements("# Doc\n\n**FR-1.1** Unrelated.\n", "FR-17"))
}

// TestParseGoalMappings_MalformedRows_Skipped covers boundary rows:
// non-goal table rows and goal rows without requirement IDs are ignored.
func TestParseGoalMappings_MalformedRows_Skipped(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		want     int
	}{
		{name: "command_table_row", markdown: "| `mgit sandbox add` | desc | flags |", want: 0},
		{name: "goal_row_without_ids", markdown: "| 3 — platform agnostic | none |", want: 0},
		{name: "goal_row_with_ids", markdown: "| 3 — platform agnostic | FR-17.2, FR-17.15 |", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Len(t, ParseGoalMappings(tt.markdown), tt.want)
		})
	}
}

// TestCountADRGoals_MissingSection_ReturnsZero covers the error path:
// an ADR without a "### Goals" section has zero countable goals, and
// counting stops at the next heading.
func TestCountADRGoals_MissingSection_ReturnsZero(t *testing.T) {
	assert.Zero(t, CountADRGoals("# ADR\n\n## Context\n\n1. not a goal\n"))
	assert.Equal(t, 2, CountADRGoals("### Goals\n\n1. one\n2. two\n\n### Non-Goals\n\n3. not counted\n"))
}
