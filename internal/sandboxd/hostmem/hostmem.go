// Package hostmem probes the host's physical memory size.
//
// It exists so mgit-sandboxd can resolve the FR-17.26 aggregate memory ceiling
// from host policy — SandboxPolicy.MaxTotalMemoryPercent, "50% of host physical
// memory across all sandboxes" — instead of from an operator flag that defaults
// to off. The policy states a PERCENT and the ceiling is enforced in MB, so
// something has to know how big the host is.
//
// The contract every probe here keeps: report a believable byte count, or
// report an error. It never guesses and never reports zero as success, because
// the caller treats zero as "no ceiling" and a plausible-but-wrong number would
// silently set the wrong ceiling. A caller that receives an error MUST fail
// closed to a conservative absolute, never to unlimited.
//
// Refs: FR-17.26, SEC-09, MGIT-98
package hostmem

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// meminfoUnit is the only unit the Linux kernel emits for MemTotal. A parse
// that met any other unit would be off by an unknown factor, so it fails
// closed rather than assuming. Refs: FR-17.26
const meminfoUnit = "kB"

// totalBytesFromMeminfo reads MemTotal from a /proc/meminfo-formatted file.
// Split from the linux-tagged TotalBytes so the read and parse paths are
// testable on a developer machine that has no /proc. Refs: FR-17.26
func totalBytesFromMeminfo(path string) (uint64, error) {
	f, err := os.Open(path) //nolint:gosec // OK: the path is a compile-time constant in production; the parameter exists only so tests can supply captured meminfo content
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parseMemTotal(f)
}

// parseMemTotal extracts MemTotal from /proc/meminfo content and converts it to
// bytes. The line looks like "MemTotal:       16311456 kB"; the value is
// scanned rather than assumed to be first, since the field order is kernel
// version dependent. Refs: FR-17.26
func parseMemTotal(r io.Reader) (uint64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		rest, found := strings.CutPrefix(scanner.Text(), "MemTotal:")
		if !found {
			continue
		}
		return memTotalBytes(strings.Fields(rest))
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read meminfo: %w", err)
	}
	return 0, fmt.Errorf("meminfo has no MemTotal line")
}

// memTotalBytes converts the parsed "<value> kB" fields to bytes.
func memTotalBytes(fields []string) (uint64, error) {
	if len(fields) != 2 || fields[1] != meminfoUnit {
		return 0, fmt.Errorf("meminfo MemTotal has an unexpected unit (want %q): %q",
			meminfoUnit, strings.Join(fields, " "))
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse meminfo MemTotal value %q: %w", fields[0], err)
	}
	if kb == 0 {
		return 0, fmt.Errorf("meminfo reports MemTotal of 0 kB, which is not a host mgit can size against")
	}
	if kb > math.MaxUint64/1024 {
		return 0, fmt.Errorf("meminfo MemTotal of %d kB overflows a byte count", kb)
	}
	return kb * 1024, nil
}
