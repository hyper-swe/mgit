//go:build !unix

package libkrun

import (
	"errors"
	"time"
)

// Windows has no sandbox backend (FR-17 v1 is Linux + macOS), and this package
// is compiled there only because cmd/mgit-sandboxd references ChildCommand. The
// lifeline is therefore never installed: fdIsPipe refuses every descriptor, so
// the provenance check fails at its first gate and no VM child watches
// anything.
//
// Refusing is the honest shape. The alternative — a stub that returns "yes,
// it's a pipe" so the code reads symmetrically — would claim supervision this
// platform does not have, which is the exact inference-by-symmetry this
// project keeps paying for. Refs: MGIT-103

// fdIsPipe reports whether fd is a pipe. Always false here: there is no VM
// child on this platform to hold one.
func fdIsPipe(int) bool { return false }

// readFDExactly is unreachable on this platform (fdIsPipe gates it) and
// refuses rather than pretending to read.
func readFDExactly(int, int, time.Duration) ([]byte, error) {
	return nil, errors.New("vm lifeline: not supported on this platform")
}
