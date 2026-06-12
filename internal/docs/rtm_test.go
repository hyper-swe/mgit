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
	defined := definedFR17Criteria(reqs)

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

// traceabilityRe identifies criteria that are themselves trace artifacts
// (the goal and finding tables), which need no inbound trace reference.
var traceabilityRe = regexp.MustCompile(`(?i)traceability`)

// fr17Criteria returns all FR-17 and NFR-17 criteria defined in the
// given REQUIREMENTS.md content.
func fr17Criteria(reqs string) []Requirement {
	out := make([]Requirement, 0, 64)
	for _, prefix := range []string{"FR-17", "NFR-17"} {
		out = append(out, ParseRequirements(reqs, prefix)...)
	}
	return out
}

// definedFR17Criteria returns the set of FR-17/NFR-17 criterion IDs
// defined in REQUIREMENTS.md.
func definedFR17Criteria(reqs string) map[string]bool {
	defined := make(map[string]bool)
	for _, c := range fr17Criteria(reqs) {
		defined[c.ID] = true
	}
	return defined
}

// collectReferenced marks every requirement ID in a traceability
// mapping as referenced.
func collectReferenced[K comparable](referenced map[string]bool, mappings map[K][]string) {
	for _, ids := range mappings {
		for _, id := range ids {
			referenced[id] = true
		}
	}
}

// assertFindingsMapped verifies every finding ID found in the audit
// document maps to >=1 defined requirement criterion via the finding
// traceability table in REQUIREMENTS.md. Refs: FR-17, MGIT-11.1.2
func assertFindingsMapped(t *testing.T, auditFile, prefix string, minFindings int) {
	t.Helper()
	reqs := readRepoFile(t, "REQUIREMENTS.md")
	audit := readRepoFile(t, auditFile)

	findings := ListFindingIDs(audit, prefix)
	require.GreaterOrEqual(t, len(findings), minFindings,
		"%s must declare at least %d %s findings", auditFile, minFindings, prefix)

	mappings := ParseFindingMappings(reqs)
	defined := definedFR17Criteria(reqs)

	for _, finding := range findings {
		ids := mappings[finding]
		assert.NotEmpty(t, ids, "audit finding %s must map to >=1 requirement criterion", finding)
		for _, id := range ids {
			assert.True(t, defined[id],
				"finding %s maps to %s, which must be a defined criterion", finding, id)
		}
	}
}

// TestTrace_AllSECFindingsMapped verifies every SEC finding in the
// security audit is encoded as a requirement. Refs: FR-17, MGIT-11.1.2
func TestTrace_AllSECFindingsMapped(t *testing.T) {
	assertFindingsMapped(t, "AUDIT-FR17-SANDBOX-SECURITY-V1.md", "SEC", 12)
}

// TestTrace_AllFFindingsMapped verifies every F finding in the
// standards audit is encoded as a requirement. Refs: FR-17, MGIT-11.1.2
func TestTrace_AllFFindingsMapped(t *testing.T) {
	assertFindingsMapped(t, "AUDIT-FR17-SANDBOX-V1.md", "F", 12)
}

// TestTrace_NoOrphanRequirements verifies bidirectional traceability:
// every requirement ID referenced from a traceability table is a defined
// criterion, and every defined FR-17/NFR-17 criterion is referenced by
// the goal table or the finding table (traceability tables themselves
// excluded). Refs: FR-17, MGIT-11.1.2
func TestTrace_NoOrphanRequirements(t *testing.T) {
	reqs := readRepoFile(t, "REQUIREMENTS.md")
	criteria := fr17Criteria(reqs)
	defined := make(map[string]bool, len(criteria))
	for _, c := range criteria {
		defined[c.ID] = true
	}

	referenced := make(map[string]bool)
	collectReferenced(referenced, ParseGoalMappings(reqs))
	collectReferenced(referenced, ParseFindingMappings(reqs))
	require.NotEmpty(t, referenced, "traceability tables must exist")

	for id := range referenced {
		if strings.HasPrefix(id, "FR-17.") || strings.HasPrefix(id, "NFR-17.") {
			assert.True(t, defined[id], "referenced %s must be a defined criterion", id)
		}
	}

	for _, c := range criteria {
		if traceabilityRe.MatchString(c.Text) {
			continue // the tables themselves are the trace artifacts
		}
		assert.True(t, referenced[c.ID],
			"criterion %s must be referenced by a traceability table (orphan)", c.ID)
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
