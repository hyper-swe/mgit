//go:build darwin

package hostmem

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// hwMemsize is the macOS sysctl reporting installed physical memory in BYTES.
// It is the 64-bit MIB; the legacy 32-bit hw.physmem saturates at 4 GiB and
// would understate every modern Mac, so it is deliberately not used.
const hwMemsize = "hw.memsize"

// TotalBytes reports the host's physical memory in bytes via sysctl
// hw.memsize. A zero reading is treated as a probe failure rather than as a
// zero-memory host: the caller must fail closed to a conservative ceiling, not
// to "no ceiling". Refs: FR-17.26, MGIT-98
func TotalBytes() (uint64, error) {
	total, err := unix.SysctlUint64(hwMemsize)
	if err != nil {
		return 0, fmt.Errorf("read sysctl %s: %w", hwMemsize, err)
	}
	if total == 0 {
		return 0, fmt.Errorf("sysctl %s reported 0 bytes", hwMemsize)
	}
	return total, nil
}
