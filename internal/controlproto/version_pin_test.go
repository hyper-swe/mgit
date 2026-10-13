package controlproto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The wire version is lockstep: a CLI and daemon must state the same number
// or refuse each other at the handshake (MGIT-136). A bump is therefore a
// deliberate act with a named cause, and this pin makes an accidental one
// impossible to land silently — whoever changes the number changes this
// test and says why. 6 = the launch options' no_guest_repo (FEAT-6.114.1); 5 was
// SandboxInfo's last_boot_failure (MGIT-231); 4 was the sync-verify verb
// (MGIT-164). Refs: MGIT-136, MGIT-231, FEAT-6.114.1
func TestProtocolVersion_IsPinned(t *testing.T) {
	assert.Equal(t, 6, ProtocolVersion,
		"ProtocolVersion moved — name the verb or shape that justifies it here, and in handshake.go's history")
	assert.True(t, Compatible(6))
	assert.False(t, Compatible(5), "the previous version must be refused, not tolerated")
}
