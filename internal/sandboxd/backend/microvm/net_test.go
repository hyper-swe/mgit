package microvm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGuestMAC_DeterministicLocallyAdministered verifies the guest MAC is
// stable per sandbox and uses a locally-administered unicast address.
// Refs: FR-17.7, SEC-05
func TestGuestMAC_DeterministicLocallyAdministered(t *testing.T) {
	m1 := GuestMAC("01JABCDEF0123456789KLMNOPQ")
	m1b := GuestMAC("01JABCDEF0123456789KLMNOPQ")
	m2 := GuestMAC("01JZZZZZZ0123456789KLMNOPQ")
	assert.Equal(t, m1, m1b)
	assert.NotEqual(t, m1, m2)

	assert.True(t, strings.HasPrefix(m1, "02:"),
		"locally-administered, unicast (02 prefix), got %s", m1[:2])
	assert.Len(t, strings.Split(m1, ":"), 6, "six octets")
}
