//go:build linux || darwin

package artifactexport

import "golang.org/x/sys/unix"

// recordedStatValueMax bounds the record an export will read. A well-formed
// value is under 40 bytes ("<uid>:<gid>:<mode>"); anything larger is not one,
// and the buffer is fixed so a guest cannot make the export allocate.
const recordedStatValueMax = 128

// readRecordedStat returns the backend's stat record for one staged path, or
// "" when there is none (the ordinary case: every host-created file, and every
// file on a backend whose share carries real modes).
//
// The lookup does NOT follow symlinks — the export reads a link's target text
// and never its referent, and resolving here would read an attribute off a
// file the host never named. Any error means "no record", which falls back to
// the host's own permission bits: an unreadable attribute must degrade to the
// pre-MGIT-81 behavior, never to a refusal or to an invented mode.
// Refs: MGIT-81
func readRecordedStat(path string) string {
	var buf [recordedStatValueMax]byte
	n, err := unix.Lgetxattr(path, recordedStatXattr, buf[:])
	if err != nil || n <= 0 || n > len(buf) {
		return ""
	}
	return string(buf[:n])
}
