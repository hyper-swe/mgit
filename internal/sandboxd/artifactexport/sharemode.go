package artifactexport

import (
	"io/fs"
	"strconv"
	"strings"
)

// Mode provenance. An exported file's mode is only ever a mode that was
// OBSERVED — the export never guesses one — but there are two places the host
// can observe it, and a consumer of the sidecar is entitled to know which.
// Refs: MGIT-81, ADR-011
const (
	// ModeSourceHostStat means the mode is what a plain host Lstat of the
	// staged file reported. It is the default and is left implicit in the
	// sidecar (an absent mode_source means this).
	ModeSourceHostStat = "host-stat"

	// ModeSourceShareRecord means the mode came from the record the sandbox's
	// virtio-fs backend keeps for a guest-created inode, because the backend
	// does not carry the mode in the host file's own permission bits.
	//
	// This is a HOST-side observation, not guest participation: the record is
	// written by the backend's host-side filesystem device as it services the
	// guest's create/chmod, exactly as the permission bits are on a backend
	// that carries them. The guest never learns an export happened.
	// Refs: MGIT-81, MGIT-73
	ModeSourceShareRecord = "share-record"
)

// recordedStatXattr is the extended attribute a virtio-fs backend uses to
// record the uid, gid and st_mode of an inode whose real host permissions
// cannot express them.
//
// It is the containers-ecosystem convention (crun, podman's rootless storage,
// and libkrun's macOS filesystem device all write this name), which is why it
// is matched by name rather than negotiated: it is a fact about the staged
// tree on disk, so an export can read it without asking any backend anything.
//
// MEASURED (MGIT-81, 2026-08-10, libkrun 1.19.4 on macOS/HVF): a guest that
// writes a 0755 file through the share leaves a host file whose own mode is
// 0600 (directories 0700) carrying this attribute with the value "0:0:0100755".
// Virtualization.framework's virtio-fs (the vzf backend) carries modes in the
// permission bits and writes no such record, so this lookup finds nothing
// there and the plain stat stands.
const recordedStatXattr = "user.containers.override_stat"

// observedMode reports the mode the host can observe for one staged file, and
// where that observation came from.
//
// Preference order is deliberate: a backend that keeps a stat record does so
// precisely BECAUSE its host permission bits are a placeholder, so the record
// is the more faithful of the two observations, and where there is no record
// the host's own bits are the only observation there is. Neither branch
// invents anything.
//
// Only PERMISSION bits are taken. The record's uid/gid are ignored (an
// exported artifact belongs to the exporting host user) and so are its type
// bits: what a file IS — regular, symlink, directory — is decided by the host
// stat alone, so a hostile record cannot talk the export into treating a
// symlink as a regular file and sidestepping the escape checks. setuid, setgid
// and sticky are dropped with everything above 0777, as they always have been.
// Refs: MGIT-81, SEC-03
func observedMode(path string, info fs.FileInfo) (fs.FileMode, string) {
	if recorded, ok := parseRecordedStat(readRecordedStat(path)); ok {
		return recorded, ModeSourceShareRecord
	}
	return info.Mode().Perm(), ModeSourceHostStat
}

// parseRecordedStat extracts the permission bits from a "<uid>:<gid>:<mode>"
// record, reporting whether the value was a well-formed record at all.
//
// The value is guest-influenced (a guest chmod is what causes the backend to
// rewrite it), so it is parsed strictly and masked hard: a malformed record is
// ignored rather than half-interpreted, and the widest thing a well-formed one
// can ask for is 0777 — which a guest could equally have asked for through an
// honest chmod on a backend that carries modes. Refs: MGIT-81
func parseRecordedStat(value string) (fs.FileMode, bool) {
	fields := strings.Split(value, ":")
	if len(fields) != 3 {
		return 0, false
	}
	mode, err := strconv.ParseUint(fields[2], 8, 32)
	if err != nil {
		return 0, false
	}
	return fs.FileMode(mode) & fs.ModePerm, true
}
