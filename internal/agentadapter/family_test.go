package agentadapter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFamilies_EveryFamilyDeclaresItsMechanismAndTier(t *testing.T) {
	fams := Families()
	require.NotEmpty(t, fams)
	seen := map[string]bool{}
	for _, f := range fams {
		t.Run(f.ID, func(t *testing.T) {
			assert.False(t, seen[f.ID], "duplicate family ID")
			seen[f.ID] = true
			assert.NotEmpty(t, f.Display)
			assert.NotEmpty(t, f.Config, "a family must name the file carrying its mechanism")
			assert.NotEmpty(t, f.Mechanism)
			assert.Contains(t, []Routing{RoutingRouted, RoutingBlocked, RoutingAdvisory}, f.Routing)
		})
	}
}

func TestLookupFamily_KnownAndUnknown(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		wantOK   bool
		wantTier Routing
	}{
		{name: "claude_is_routed", id: "claude", wantOK: true, wantTier: RoutingRouted},
		{name: "codex_is_routed", id: "codex", wantOK: true, wantTier: RoutingRouted},
		{name: "cursor_is_blocked_not_routed", id: "cursor", wantOK: true, wantTier: RoutingBlocked},
		{name: "generic_is_advisory", id: "generic", wantOK: true, wantTier: RoutingAdvisory},
		{name: "case_insensitive", id: "Claude", wantOK: true, wantTier: RoutingRouted},
		{name: "unknown_family", id: "nosuchagent", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, ok := LookupFamily(tt.id)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantTier, f.Routing)
			}
		})
	}
}

// The report is the thing that ends the silence: an operator reading it must be
// able to tell, per family and BY NAME, whether routing is enforced or advised.
// Refs: MGIT-149
func TestRoutingReport_NamesEveryFamilyAndItsTier(t *testing.T) {
	got := RoutingReport()
	for _, f := range Families() {
		assert.Contains(t, got, f.Display, "report must name the family")
		assert.Contains(t, got, f.Config, "report must name where the mechanism lives")
	}
	assert.Contains(t, got, "advisory", "the advisory lane must be called advisory in plain words")
}

// A machine-parseable line per family, so a harness or script can read the
// posture without parsing prose. Refs: MGIT-149, MGIT-47
func TestRoutingStatusLines_AreMachineParseableAndCoverEveryFamily(t *testing.T) {
	lines := RoutingStatusLines()
	require.Len(t, lines, len(Families()))
	for i, f := range Families() {
		assert.True(t, strings.HasPrefix(lines[i], "Routing: "), "line %d: %q", i, lines[i])
		assert.Contains(t, lines[i], f.ID+"=")
	}
}

// The advisory lane must never be described in words that imply enforcement.
// This is the pin for the whole ticket: the failure being fixed is that an
// advisory lane looked identical to an enforced one. Refs: MGIT-149
func TestRoutingReport_DoesNotDescribeTheAdvisoryLaneAsEnforced(t *testing.T) {
	report := RoutingReport()
	generic, ok := LookupFamily("generic")
	require.True(t, ok)
	idx := strings.Index(report, generic.Display)
	require.GreaterOrEqual(t, idx, 0)
	// The sentence describing the generic family must say advisory, and must not
	// borrow the word "enforced" for itself.
	line := report[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	assert.Contains(t, strings.ToLower(line), "advisory")
	assert.NotContains(t, strings.ToLower(line), "enforced")
}

// Each family's instructions must describe ITS OWN tier. The failure this
// guards is symmetric to the one MGIT-149 fixes: a shared narrative that says
// "no hook routes you, prepend PATH yourself" is now FALSE for Codex and
// Cursor, and telling a routed agent it is unrouted is its own inaccuracy —
// it invites the agent to add prefixes that are already applied, and it
// understates a guarantee mgit does provide. Refs: MGIT-149, R-H299
func TestRoutingNarrative_DescribesEachFamilysOwnTier(t *testing.T) {
	tests := []struct {
		id              string
		wantContains    []string
		wantNotContains []string
	}{
		{
			id:           "codex",
			wantContains: []string{"rewrites", CodexHooksFile},
			// A routed family must not be told routing depends on it.
			wantNotContains: []string{"COOPERATIVE, not enforced", "no such hook"},
		},
		{
			id:              "cursor",
			wantContains:    []string{"refused", CursorHooksFile, "mgit run"},
			wantNotContains: []string{"COOPERATIVE, not enforced", "no such hook"},
		},
		{
			id:              "generic",
			wantContains:    []string{"COOPERATIVE", "on the host", "hostname; whoami"},
			wantNotContains: []string{"cannot miss it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			fam, ok := LookupFamily(tt.id)
			require.True(t, ok)
			got := routingNarrative("/wt/.mgit/shims", fam)
			for _, want := range tt.wantContains {
				assert.Contains(t, got, want)
			}
			for _, no := range tt.wantNotContains {
				assert.NotContains(t, got, no)
			}
		})
	}
}

// The pin from R-H299 survives the generalisation, in the form the tiers now
// require: a family may claim containment ONLY where a mechanism enforces it,
// and even then the claim must ship with the check that falsifies it.
//
// mgit installs a hook file; it cannot make a harness load one. A Codex older
// than v0.114 ignores .codex/hooks.json silently, and an agent told it is
// routed would be uncontained while believing otherwise — this ticket's defect
// wearing a different hat. Refs: MGIT-149, R-H299
func TestRoutingNarrative_ContainmentIsClaimedOnlyWithAMechanismAndACheck(t *testing.T) {
	for _, fam := range Families() {
		t.Run(fam.ID, func(t *testing.T) {
			got := routingNarrative("/wt/.mgit/shims", fam)
			// Every family, every tier: a way to check, never a bare assurance.
			assert.Contains(t, got, "hostname; whoami")
			if fam.Routing == RoutingAdvisory {
				assert.NotContains(t, got, "they are contained",
					"a family with no mechanism must not claim containment")
				return
			}
			assert.Contains(t, got, "not loaded",
				"an enforced tier must tell the agent how to detect a hook the harness never loaded")
			assert.Contains(t, got, "Stop and report",
				"detecting an unloaded hook is useless without saying what to do about it")
		})
	}
}
