package docs

// rtm.go is the Requirements Traceability Matrix (RTM) parsing stub.
// It extracts numbered requirement criteria and ADR-goal mappings from
// the project's markdown specifications so traceability can be verified
// mechanically (criterion -> code -> test). Consumed by the RTM tests
// today and by the RTM generator (FR-15, MGIT-11.12.4) later.
// Refs: FR-17, NFR-17, MGIT-11.1.1

import (
	"regexp"
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

// ParseGoalMappings extracts the ADR-goal traceability table from a
// requirements markdown document. It returns goal number -> requirement
// IDs listed in that goal's table row.
func ParseGoalMappings(markdown string) map[int][]string {
	out := make(map[int][]string)
	for _, line := range strings.Split(markdown, "\n") {
		row := goalRowRe.FindStringSubmatch(line)
		if row == nil {
			continue
		}
		goal, err := strconv.Atoi(row[1])
		if err != nil {
			continue
		}
		ids := requirementIDRe.FindAllString(line, -1)
		if len(ids) > 0 {
			out[goal] = append(out[goal], ids...)
		}
	}
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
