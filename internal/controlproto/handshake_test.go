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
		"KindExport": KindExport, "KindHello": KindHello,
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
// (MGIT-132). Refs: MGIT-136, MGIT-132
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
// skew text must not add a fifth route by so much as naming the cap.
// Refs: MGIT-136, MGIT-118
func TestSkewMessage_NeverMentionsMemory(t *testing.T) {
	msg := strings.ToLower(SkewMessage(Peer{Protocol: 2, Version: "a"}, Peer{Protocol: 1, Version: "b"}))
	for _, forbidden := range []string{"memory", "memory-mb", "out of memory", "oom"} {
		assert.NotContains(t, msg, forbidden)
	}
}
