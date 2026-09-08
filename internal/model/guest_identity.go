package model

import (
	"path"
	"regexp"
	"strings"
)

// GuestIdentity is the uid/gid an exec runs as inside the guest, with the
// name and home directory the guest materializes for them before the
// command starts.
//
// WHY A PAIR FROM THE HOST AND NOT A FIXED NUMBER. The daemon delivers the
// worktree and the composed base as itself — the libkrun share maps no ids
// and the firecracker image copies inode owners — and an unprivileged daemon
// cannot chown either to a foreign uid. So the identity a guest command can
// read the base and write the worktree as is the delivering daemon's own
// uid/gid, measured on a live guest (MGIT-151); the guest gives it a name
// and a home so `id`, `$HOME` and tools that look up the current user all
// answer. Refs: MGIT-151, FR-17.11
type GuestIdentity struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Name string `json:"name,omitempty"` // passwd name; empty lets the guest choose
	Home string `json:"home,omitempty"` // absolute; empty lets the guest choose
}

// RootIdentity is the explicit root identity an escalated exec runs as.
// Refs: MGIT-151
func RootIdentity() GuestIdentity {
	return GuestIdentity{UID: 0, GID: 0, Name: "root", Home: "/root"}
}

// IsRoot reports whether the identity is uid 0, whatever its gid.
func (g GuestIdentity) IsRoot() bool { return g.UID == 0 }

// identityNamePattern is a conservative POSIX user name: it is written into
// the guest's passwd file, so it must never carry a separator or a space.
var identityNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Validate checks the shape of an identity: non-negative ids, a passwd-safe
// name, and a clean absolute home. The values are the daemon's to choose;
// only their shape is checked. Refs: MGIT-151
func (g GuestIdentity) Validate() error {
	if g.UID < 0 {
		return &ValidationError{Field: "uid", Message: "must be non-negative"}
	}
	if g.GID < 0 {
		return &ValidationError{Field: "gid", Message: "must be non-negative"}
	}
	if g.Name != "" && !identityNamePattern.MatchString(g.Name) {
		return &ValidationError{Field: "name", Message: "must be a passwd-safe name: [a-z_][a-z0-9_-]{0,31}"}
	}
	if g.Home != "" {
		if !path.IsAbs(g.Home) || path.Clean(g.Home) != g.Home || hasDotDot(g.Home) {
			return &ValidationError{Field: "home", Message: "must be a clean absolute path without .."}
		}
	}
	return nil
}

// hasDotDot reports whether any segment of p is "..".
func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
