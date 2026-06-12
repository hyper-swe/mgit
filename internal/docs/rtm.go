package docs

// rtm.go is the Requirements Traceability Matrix (RTM) parsing stub.
// It extracts numbered requirement criteria and ADR-goal mappings from
// the project's markdown specifications so traceability can be verified
// mechanically (criterion -> code -> test). Consumed by the RTM tests
// today and by the RTM generator (FR-15, MGIT-11.12.4) later.
// Refs: FR-17, NFR-17, MGIT-11.1.1

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Requirement is a single numbered requirement criterion, e.g. FR-17.3.
type Requirement struct {
	ID   string // Stable requirement ID (e.g. "FR-17.3", "NFR-17.1")
	Text string // First line of the requirement statement
}

// criterionRe matches a criterion line: **FR-17.3** The guest MUST ...
var criterionRe = regexp.MustCompile(`(?m)^\*\*((?:N)?FR-\d+\.\d+[a-z]?)\*\*\s+(.+)$`)

// requirementIDRe matches requirement IDs inside free text or table cells.
var requirementIDRe = regexp.MustCompile(`(?:N)?FR-\d+\.\d+[a-z]?`)

// goalRowRe matches a traceability-table row whose first cell is a goal
// number: | 3 — platform agnostic | FR-17.15, FR-17.2 |
var goalRowRe = regexp.MustCompile(`^\|\s*(\d+)\s*[—-]`)

// numberedItemRe matches a numbered markdown list item: "1. Agents run ..."
var numberedItemRe = regexp.MustCompile(`(?m)^\d+\.\s+\S`)

// findingRowRe matches a traceability-table row whose first cell is an
// audit finding ID: | SEC-01 — forgeable attestation | ... | FR-17.6 |
var findingRowRe = regexp.MustCompile(`^\|\s*((?:SEC|F)-\d{2})\b`)

// findingIDRe matches audit finding IDs (SEC-01.., F-01..) in free text.
var findingIDRe = regexp.MustCompile(`\b(?:SEC|F)-\d{2}\b`)

// sectionHeadingRe matches an h2/h3 markdown heading line.
var sectionHeadingRe = regexp.MustCompile(`(?m)^#{2,3} `)

// RequirementSection returns the body of the "### <id>: ..." section of
// a requirements document, from its heading to the next h2/h3 heading.
// Mapping-table parsers must be scoped to a section: rows elsewhere in
// the document (revision histories, other FR tables) would otherwise
// merge into the traceability mappings and silently satisfy coverage
// assertions.
func RequirementSection(markdown, id string) string {
	_, rest, found := strings.Cut(markdown, "### "+id+":")
	if !found {
		return ""
	}
	if loc := sectionHeadingRe.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

// ParseRequirements extracts all criteria with the given ID prefix
// (e.g. "FR-17") from a requirements markdown document.
func ParseRequirements(markdown, prefix string) []Requirement {
	var out []Requirement
	for _, m := range criterionRe.FindAllStringSubmatch(markdown, -1) {
		if strings.HasPrefix(m[1], prefix+".") {
			out = append(out, Requirement{ID: m[1], Text: m[2]})
		}
	}
	return out
}

// parseMappingTable extracts a traceability table from markdown. Each
// line matching rowRe contributes one entry: the first capture group is
// converted by key, and the values are all requirement IDs in the row.
func parseMappingTable[K comparable](markdown string, rowRe *regexp.Regexp, key func(string) (K, bool)) map[K][]string {
	out := make(map[K][]string)
	for _, line := range strings.Split(markdown, "\n") {
		row := rowRe.FindStringSubmatch(line)
		if row == nil {
			continue
		}
		k, ok := key(row[1])
		if !ok {
			continue
		}
		if ids := requirementIDRe.FindAllString(line, -1); len(ids) > 0 {
			out[k] = append(out[k], ids...)
		}
	}
	return out
}

// ParseGoalMappings extracts the ADR-goal traceability table from a
// requirements markdown document. It returns goal number -> requirement
// IDs listed in that goal's table row.
func ParseGoalMappings(markdown string) map[int][]string {
	return parseMappingTable(markdown, goalRowRe, func(s string) (int, bool) {
		goal, err := strconv.Atoi(s)
		return goal, err == nil
	})
}

// ParseFindingMappings extracts the audit-finding traceability table
// from a requirements markdown document. It returns finding ID
// (e.g. "SEC-01", "F-04") -> requirement IDs listed in that row.
func ParseFindingMappings(markdown string) map[string][]string {
	return parseMappingTable(markdown, findingRowRe, func(s string) (string, bool) {
		return s, true
	})
}

// ListFindingIDs returns the distinct audit finding IDs with the given
// prefix ("SEC" or "F") mentioned in an audit document, sorted.
func ListFindingIDs(audit, prefix string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, id := range findingIDRe.FindAllString(audit, -1) {
		if strings.HasPrefix(id, prefix+"-") && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// CountADRGoals counts the numbered list items in the "### Goals"
// section of an ADR markdown document.
func CountADRGoals(adr string) int {
	_, rest, found := strings.Cut(adr, "### Goals")
	if !found {
		return 0
	}
	if next := strings.Index(rest, "###"); next >= 0 {
		rest = rest[:next]
	}
	return len(numberedItemRe.FindAllString(rest, -1))
}
