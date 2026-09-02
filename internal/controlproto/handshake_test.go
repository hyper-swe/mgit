package controlproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHello_RoundTrip verifies a hello request and its reply survive the wire
// unchanged — the version exchange is the one frame both ends must be able to
// read before they agree on anything else. Refs: MGIT-136
func TestHello_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteRequest(&buf, &Request{
		Kind:  KindHello,
		Hello: &HelloArgs{Protocol: ProtocolVersion, Version: "0.6.0 (commit: abc)"},
	}))
	got, err := ReadRequest(&buf)
	require.NoError(t, err)
	assert.Equal(t, KindHello, got.Kind)
	require.NotNil(t, got.Hello)
	assert.Equal(t, ProtocolVersion, got.Hello.Protocol)
	assert.Equal(t, "0.6.0 (commit: abc)", got.Hello.Version)

	buf.Reset()
	require.NoError(t, WriteResponse(&buf, &Response{
		Hello: &HelloResult{Protocol: ProtocolVersion, Version: "sandboxd 0.6.0"},
	}))
	resp, err := ReadResponse(&buf)
	require.NoError(t, err)
	require.NotNil(t, resp.Hello)
	assert.Equal(t, ProtocolVersion, resp.Hello.Protocol)
}

// TestHello_KindTagIsUnique pins the hello tag against every other request
// kind. Two parallel branches once both reached for 'P' and it surfaced only
// when they met (MGIT-73); a duplicate here would make the version handshake
// indistinguishable from a verb, which is the one frame that must never be
// mistaken for something else. Refs: MGIT-136, MGIT-73
func TestHello_KindTagIsUnique(t *testing.T) {
	kinds := map[string]byte{
		"KindLaunch": KindLaunch, "KindExec": KindExec, "KindLand": KindLand,
		"KindList": KindList, "KindRemove": KindRemove, "KindStatus": KindStatus,
		"KindGrants": KindGrants, "KindGrant": KindGrant, "KindSync": KindSync,
		"KindPolicySet": KindPolicySet, "KindPolicyShow": KindPolicyShow,
		"KindExport": KindExport, "KindHello": KindHello, "KindEcho": KindEcho,
	}
	seen := make(map[byte]string, len(kinds))
	for name, k := range kinds {
		if prior, dup := seen[k]; dup {
			t.Fatalf("request tag %#x is used by both %s and %s", k, prior, name)
		}
		seen[k] = name
		assert.True(t, validKind(k), "%s must be a valid kind", name)
	}
	assert.Len(t, seen, len(kinds))
}

// TestHello_Validate_FailsClosed verifies a malformed hello is refused at the
// protocol boundary rather than reaching the compatibility rule. Refs: MGIT-136
func TestHello_Validate_FailsClosed(t *testing.T) {
	tests := []struct {
		name string
		req  *Request
	}{
		{"missing_payload", &Request{Kind: KindHello}},
		{"protocol_zero", &Request{Kind: KindHello, Hello: &HelloArgs{Protocol: 0}}},
		{"protocol_negative", &Request{Kind: KindHello, Hello: &HelloArgs{Protocol: -1}}},
		{"version_oversize", &Request{Kind: KindHello, Hello: &HelloArgs{
			Protocol: 1, Version: strings.Repeat("v", MaxVersionBytes+1)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, tt.req.Validate())
		})
	}
}

// TestCompatible_ExactMatchOnly enforces the compatibility rule EXACTLY as
// written down: equal protocol numbers and nothing else, including both
// boundaries (one below and one above). A looser check here would be a promise
// the codec cannot keep — see the rule's rationale in handshake.go.
// Refs: MGIT-136
func TestCompatible_ExactMatchOnly(t *testing.T) {
	tests := []struct {
		name string
		peer int
		want bool
	}{
		{"same_version", ProtocolVersion, true},
		{"one_below", ProtocolVersion - 1, false},
		{"one_above", ProtocolVersion + 1, false},
		{"legacy_unversioned", LegacyProtocol, false},
		{"absent", 0, false},
		{"far_future", ProtocolVersion + 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Compatible(tt.peer))
		})
	}
}

// TestProtocolVersion_IsAheadOfLegacy pins the two constants apart: the legacy
// number names every pre-handshake build, so a ProtocolVersion that collided
// with it would report every old daemon as compatible. Refs: MGIT-136
func TestProtocolVersion_IsAheadOfLegacy(t *testing.T) {
	assert.Greater(t, ProtocolVersion, LegacyProtocol)
}

// TestSkewMessage_NamesBothSidesAndTheRemedy is the message contract: a reader
// must learn which side is which version and what command to run, without
// interpreting a version number or hunting for install instructions
// (MGIT-132). The pair here is the DAEMON-stale direction, which is the one
// that ends in `pkill`; the CLI-stale direction is asserted separately.
// Refs: MGIT-138, MGIT-136, MGIT-132
func TestSkewMessage_NamesBothSidesAndTheRemedy(t *testing.T) {
	msg := SkewMessage(Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"},
		Peer{Protocol: 1, Version: "0.5.0 (commit: bbb)"})

	assert.Contains(t, msg, "CLI and daemon differ", "the headline states the fault plainly")
	assert.Contains(t, msg, "upgrade both")
	assert.Contains(t, msg, "0.6.0 (commit: aaa)", "the CLI's build is named")
	assert.Contains(t, msg, "0.5.0 (commit: bbb)", "the daemon's build is named")
	assert.Contains(t, msg, "mgit CLI", "which side is which is stated, not implied")
	assert.Contains(t, msg, "mgit-sandboxd")

	// Every install route this project actually ships (MGIT-132: a remedy
	// naming a command the reader cannot run is the defect one level up).
	for _, want := range []string{
		"install.sh",
		"brew upgrade hyper-swe/tap/mgit",
		"go install github.com/hyper-swe/mgit/cmd/mgit@latest",
		"go build -o",
		"pkill -f mgit-sandboxd",
		"mgit --version",
		"mgit-sandboxd --version",
	} {
		assert.Contains(t, msg, want, "the remedy must name %q", want)
	}
}

// TestSkewMessage_StaleSide_DecidesTheClosingRemedy is MGIT-138's second half.
//
// Both peers exchange both versions, so both already know which binary is
// behind — and both used to print the same closing line regardless: "stop the
// daemon still running the old build: pkill -f mgit-sandboxd". Exact when the
// daemon is stale; superfluous when the CLI is, which is the direction verified
// live (an mgit 0.5.0 CLI meeting a current daemon), where it sends the reader
// after a process that is not the problem and leaves the real fix unsaid.
//
// The NEGATIVE is the assertion that matters here: a remedy aimed at the wrong
// side is a mild member of the misdiagnosis family this project has closed four
// routes into (MGIT-104, MGIT-108, MGIT-118, MGIT-136), so the pkill line must
// be ABSENT — not merely accompanied by better advice — when the CLI is the old
// half. Refs: MGIT-138, MGIT-136, MGIT-132
func TestSkewMessage_StaleSide_DecidesTheClosingRemedy(t *testing.T) {
	tests := []struct {
		name          string
		cli, daemon   Peer
		wantPkill     bool
		wantSubstring string
	}{
		{
			name:          "daemon_is_stale",
			cli:           Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"},
			daemon:        Peer{Protocol: LegacyProtocol},
			wantPkill:     true,
			wantSubstring: "It is the one serving THIS repository",
		},
		{
			name:          "cli_is_stale",
			cli:           Peer{Protocol: LegacyProtocol},
			daemon:        Peer{Protocol: 2, Version: "0.6.0 (commit: bbb)"},
			wantPkill:     false,
			wantSubstring: "The stale half here is the mgit CLI",
		},
		{
			name:          "cli_is_stale_by_a_future_daemon",
			cli:           Peer{Protocol: 2, Version: "0.6.0"},
			daemon:        Peer{Protocol: 3, Version: "0.7.0"},
			wantPkill:     false,
			wantSubstring: "upgrading the CLI is the whole fix",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := SkewMessage(tt.cli, tt.daemon)
			assert.Contains(t, msg, tt.wantSubstring)
			if tt.wantPkill {
				assert.Contains(t, msg, "--repo-root $(git rev-parse --show-toplevel)",
					"the daemon is the stale half and the reader is not told to replace it")
				// The kill must be SCOPED. A bare `pkill -f mgit-sandboxd` matches
				// every repository's daemon on the host, since sockets are keyed per
				// repo root — so the remedy would stop unrelated daemons and the
				// microVMs they supervise. A remedy that damages what it was not
				// aimed at is the same defect class this message exists to avoid,
				// and it is what this project's own working rules forbid. This
				// asserts the bare form appears ONLY inside the sentence warning
				// against it. Refs: MGIT-141, MGIT-138
				bare := strings.Count(msg, "pkill -f mgit-sandboxd\n")
				assert.Zero(t, bare,
					"the remedy tells the reader to run an unscoped pkill, which stops "+
						"every repo's daemon, not the stale one this message is about")
				assert.Contains(t, msg, "would also stop other repositories' daemons",
					"the unscoped form is mentioned without warning what it does")
				return
			}
			assert.NotContains(t, msg, "pkill",
				"the CLI is the stale half, so the reader was sent after a daemon that is "+
					"already current — a remedy aimed at the wrong side")
			assert.NotContains(t, msg, "stop the daemon",
				"nothing about the running daemon needs stopping when the CLI is the old half")
		})
	}
}

// TestSkewMessage_EqualProtocols_NameNeitherSideStale covers the one input
// staleSideRemedy has no answer for.
//
// Equal protocol numbers are not a mismatch — Compatible is equality, and this
// message is rendered only when it fails — so this pair cannot reach the
// message in production. The branch is not there to handle a peer; it is there
// so the aiming rule is TOTAL: with no evidence of which side is behind, the
// message states neither rather than picking one. Naming a stale side from
// numbers that say nothing is precisely the wrong-side remedy MGIT-138 removed.
// Refs: MGIT-138
func TestSkewMessage_EqualProtocols_NameNeitherSideStale(t *testing.T) {
	msg := SkewMessage(Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"},
		Peer{Protocol: 2, Version: "0.6.0 (commit: bbb)"})

	assert.NotContains(t, msg, "pkill", "the daemon was named stale on no evidence")
	assert.NotContains(t, msg, "The stale half here is the mgit CLI",
		"the CLI was named stale on no evidence")
	// The rest of the message is unaffected: it still names both builds and
	// still ends with the confirm line.
	assert.Contains(t, msg, "0.6.0 (commit: aaa)")
	assert.Contains(t, msg, "0.6.0 (commit: bbb)")
	assert.True(t, strings.HasSuffix(msg, "mgit --version; mgit-sandboxd --version"))
}

// TestSkewMessage_BothDirections_KeepEveryOtherGuarantee holds the rest of the
// contract steady across the split: whichever side is stale, the message still
// names BOTH builds, still lists only install routes this project has, and
// still ends with the two --version commands MGIT-132 asks bug reports to
// quote. Refs: MGIT-138, MGIT-136, MGIT-132
func TestSkewMessage_BothDirections_KeepEveryOtherGuarantee(t *testing.T) {
	directions := map[string]string{
		"daemon_is_stale": SkewMessage(
			Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"},
			Peer{Protocol: 1, Version: "0.5.0 (commit: bbb)"}),
		"cli_is_stale": SkewMessage(
			Peer{Protocol: 1, Version: "0.5.0 (commit: bbb)"},
			Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"}),
	}
	for name, msg := range directions {
		t.Run(name, func(t *testing.T) {
			for _, want := range []string{
				"CLI and daemon differ", "upgrade both",
				"0.6.0 (commit: aaa)", "0.5.0 (commit: bbb)",
				"mgit CLI:", "mgit-sandboxd:",
				"install.sh",
				"brew upgrade hyper-swe/tap/mgit",
				"go install github.com/hyper-swe/mgit/cmd/mgit@latest",
				"go build -o",
			} {
				assert.Contains(t, msg, want, "the message must still name %q", want)
			}
			assert.True(t, strings.HasSuffix(msg,
				"mgit --version; mgit-sandboxd --version"),
				"MGIT-132's confirm-and-quote line must remain the last thing the reader sees")
			// Routes this project does NOT have: naming one is the defect
			// MGIT-132 is about, one level up.
			assert.NotContains(t, msg, "make install",
				"there is no make install target")
			assert.NotContains(t, msg, "make build",
				"make build builds only the CLI; it cannot fix a mixed pair")
		})
	}
}

// TestSkewMessage_UnknownPeerVersion_StillReadable verifies the message stays
// honest when a pre-handshake peer could not state its build: it says the
// protocol it must be speaking rather than inventing a version. Refs: MGIT-136
func TestSkewMessage_UnknownPeerVersion_StillReadable(t *testing.T) {
	msg := SkewMessage(Peer{Protocol: 2, Version: "0.6.0"}, Peer{Protocol: LegacyProtocol})
	assert.Contains(t, msg, "control protocol 1")
	assert.Contains(t, msg, unknownBuild)
	assert.NotContains(t, msg, "()", "an absent build must not render as empty parentheses")
}

// TestSkewMessage_NeverMentionsMemory is the negative this ticket exists for.
// Four times now a version or host-side fault has been reported to an agent as
// in-guest memory exhaustion (MGIT-104, MGIT-108, MGIT-118, MGIT-136); the
// skew text must not add a fifth route by so much as naming the cap. Asserted
// in BOTH stale directions, since MGIT-138 made them different strings.
// Refs: MGIT-138, MGIT-136, MGIT-118
func TestSkewMessage_NeverMentionsMemory(t *testing.T) {
	for _, msg := range []string{
		SkewMessage(Peer{Protocol: 2, Version: "a"}, Peer{Protocol: 1, Version: "b"}),
		SkewMessage(Peer{Protocol: 1, Version: "b"}, Peer{Protocol: 2, Version: "a"}),
	} {
		lower := strings.ToLower(msg)
		for _, forbidden := range []string{"memory", "memory-mb", "out of memory", "oom", "capped at"} {
			assert.NotContains(t, lower, forbidden)
		}
	}
}
