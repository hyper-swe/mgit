//go:build linux

package main

import (
	"fmt"
	"os"
)

// shadowRecordFile is where the guest leaves the record of a repaired mount
// point. /tmp is a tmpfs by the time the worktree is mounted, so writing it
// needs no copy-up. Refs: MGIT-230.8
const shadowRecordFile = "/tmp/.mgit-mount-point-shadowed"

// writeShadowRecord writes one line, "dir=<shadowed directory> path=<mount
// point>", to file, world-readable so the unprivileged user a command runs as
// can read it. The Linux user-path leg reads it back through `mgit run`.
func writeShadowRecord(file, dir, path string) error {
	//nolint:gosec // G306: a world-readable evidence line, no secret in it.
	return os.WriteFile(file, []byte(fmt.Sprintf("dir=%s path=%s\n", dir, path)), 0o644)
}
