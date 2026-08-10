//go:build unix

package artifactexport

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireFIFO creates a named pipe, the cheapest irregular file to plant in a
// staged tree: exporting one would either block forever or produce a host file
// that is not what the guest had.
func requireFIFO(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, unix.Mkfifo(path, 0o600))
}
