//go:build !unix

package artifactexport

import "testing"

// requireFIFO has no portable equivalent off unix; the irregular-file case is
// covered on the platforms artifact export actually runs on.
func requireFIFO(t *testing.T, _ string) {
	t.Helper()
	t.Skip("SKIP (irregular file): named pipes are a unix construct")
}
