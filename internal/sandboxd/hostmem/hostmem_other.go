//go:build !linux && !darwin

package hostmem

import (
	"fmt"
	"runtime"
)

// TotalBytes reports that this platform has no host physical-memory probe.
//
// It is an ERROR rather than a zero on purpose. Zero is the caller's encoding
// for "this ceiling dimension is disabled", so returning it here would turn an
// unprobeable platform into an unlimited one — the exact fail-open this whole
// mechanism exists to prevent. The caller fails closed to a conservative
// absolute instead. Windows lands here until the WCOW backend ships (ADR-006);
// the sandbox is Linux + macOS only in v1, so no supported sandbox host is
// affected today. Refs: FR-17.26, ADR-006, MGIT-98
func TotalBytes() (uint64, error) {
	return 0, fmt.Errorf("no host physical-memory probe for %s/%s", runtime.GOOS, runtime.GOARCH)
}
