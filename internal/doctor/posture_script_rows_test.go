package doctor

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The posture gate asserts doctor rows BY NAME. This is the sweep the
// preflight law asks for, from the Go side: every row name the script
// mentions must be a check this package registers, so a rename in Go fails
// here before it silently turns the shell gate into a MISSING. Refs: MGIT-195
func TestPostureScript_NamesOnlyRealDoctorRows(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "e2e", "sandbox_posture.sh"))
	require.NoError(t, err)
	known := map[string]bool{}
	for _, c := range []Check{GuestLocalhostCheck{}, BaseCurrencyCheck{}, GuestSyncVerifyCheck{}, GuestDeliveryCheck{}, HostDaemonsCheck{}, DuplicateDaemonsCheck{}} {
		known[c.Name()] = true
	}
	mentioned := regexp.MustCompile(`expect_row "\$[a-z]+" ([a-z/-]+) `).FindAllStringSubmatch(string(script), -1)
	require.NotEmpty(t, mentioned, "the posture script asserts doctor rows")
	for _, m := range mentioned {
		assert.True(t, known[m[1]], "the posture script names doctor row %q, which no check registers", m[1])
	}
}
