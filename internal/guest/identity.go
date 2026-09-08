package guest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// Defaults for an identity the host named only by number: the guest's own
// convention for the unprivileged user commands run as. Refs: MGIT-151
const (
	defaultIdentityName     = "agent"
	defaultIdentityHome     = "/home/agent"
	defaultEtcDir           = "/etc"
	defaultFallbackHomeRoot = "/tmp/home"
)

// withIdentityDefaults fills the name and home the host left empty.
func withIdentityDefaults(id model.GuestIdentity) model.GuestIdentity {
	root := model.RootIdentity()
	if id.Name == "" {
		id.Name = defaultIdentityName
		if id.IsRoot() {
			id.Name = root.Name
		}
	}
	if id.Home == "" {
		id.Home = defaultIdentityHome
		if id.IsRoot() {
			id.Home = root.Home
		}
	}
	return id
}

// processIdentity is the identity this supervisor itself runs as — what a
// command runs as when the host asks for nothing.
func processIdentity() model.GuestIdentity {
	return model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid()}
}

// ensureIdentity materializes a non-root identity before a command is run
// as it: a passwd line and a group line PREPENDED to the guest's files so
// the uid and gid resolve to the identity's name rather than to whatever
// distro entry happens to share the number (a debian base answers gid 20
// as `dialout` otherwise), and a home that exists and is owned by it.
// Idempotent by NAME: the lines are written once, and a distro entry that
// shares the number is kept behind them.
// Everything here runs as the supervisor (root in a guest); a failure
// refuses the exec rather than letting it run under a half-made identity.
// Refs: MGIT-151
func (s *Supervisor) ensureIdentity(id model.GuestIdentity) (model.GuestIdentity, error) {
	if id.IsRoot() {
		// Root needs no entries, but its home must exist too: a minimal base
		// ships no /root, and CI's root half found HOME pointing at nothing.
		return s.ensureHome(id)
	}
	etc := s.EtcDir
	if etc == "" {
		etc = defaultEtcDir
	}
	// A minimal base may ship no etc directory at all; the entries need one.
	if err := os.MkdirAll(etc, 0o755); err != nil { //nolint:gosec // G301: /etc is world-readable by design
		return id, fmt.Errorf("etc dir %s: %w", etc, err)
	}
	passwdLine := fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh", id.Name, id.UID, id.GID, id.Home)
	if err := prependEntryUnlessName(filepath.Join(etc, "passwd"), id.Name, passwdLine); err != nil {
		return id, fmt.Errorf("passwd entry for uid %d: %w", id.UID, err)
	}
	groupLine := fmt.Sprintf("%s:x:%d:", id.Name, id.GID)
	if err := prependEntryUnlessName(filepath.Join(etc, "group"), id.Name, groupLine); err != nil {
		return id, fmt.Errorf("group entry for gid %d: %w", id.GID, err)
	}
	return s.ensureHome(id)
}

// ensureHome gives the identity a home that exists and is owned by it, and
// returns the identity with the home it actually has. A home that already
// exists with the right owner is left exactly as it is: no chown, which the
// Linux/libkrun overlay refuses even for root's own /root (MGIT-89) — CI's
// libkrun leg refused every exec on that chown. A home the guest's root
// cannot take falls back under FallbackHomeRoot (/tmp/home by default),
// which every guest can write; the child's HOME and the echoed identity
// name the fallback. Refs: MGIT-151, MGIT-89
func (s *Supervisor) ensureHome(id model.GuestIdentity) (model.GuestIdentity, error) {
	if ownedDir(id.Home, id) {
		return id, nil
	}
	firstErr := makeOwnedHome(id.Home, id)
	if firstErr == nil {
		return id, nil
	}
	root := s.FallbackHomeRoot
	if root == "" {
		root = defaultFallbackHomeRoot
	}
	fallback := filepath.Join(root, id.Name)
	if ownedDir(fallback, id) {
		id.Home = fallback
		return id, nil
	}
	if err := makeOwnedHome(fallback, id); err != nil {
		return id, fmt.Errorf("home %s: %w; fallback %s: %w", id.Home, firstErr, fallback, err)
	}
	if s.Logger != nil {
		s.Logger.Info("mgit-guest identity home fell back", "event", "identity_home_fallback",
			"asked", id.Home, "home", fallback, "reason", firstErr.Error())
	}
	id.Home = fallback
	return id, nil
}

// ownedDir reports whether path is a directory owned by the identity.
func ownedDir(path string, id model.GuestIdentity) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	uid, gid, ok := fileOwner(fi)
	return ok && uid == id.UID && gid == id.GID
}

// makeOwnedHome creates the home — parents any identity can traverse, the
// home itself owner-only (the live libkrun proof found `/home` created 0750
// by the root supervisor, so the identity resolved its home and could not
// write a byte into it) — and owns it to the identity.
func makeOwnedHome(home string, id model.GuestIdentity) error {
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil { //nolint:gosec // G301: parents must be traversable by the identity
		return fmt.Errorf("home parent for %s: %w", home, err)
	}
	if err := os.Mkdir(home, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("home %s: %w", home, err)
	}
	if ownedDir(home, id) {
		return nil
	}
	if err := os.Chown(home, id.UID, id.GID); err != nil {
		return fmt.Errorf("own home %s: %w", home, err)
	}
	return nil
}

// identityEnv is the environment the identity implies for the child.
func identityEnv(id model.GuestIdentity) []string {
	return []string{"HOME=" + id.Home, "USER=" + id.Name, "LOGNAME=" + id.Name}
}

// prependEntryUnlessName prepends line to the colon-separated file at path
// unless an entry of that name already exists. A missing file is created.
// Refs: MGIT-151
func prependEntryUnlessName(path, name, line string) error {
	existing, err := os.ReadFile(path) //nolint:gosec // a fixed passwd/group path under the guest's etc dir
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.HasPrefix(l, name+":") {
			return nil
		}
	}
	content := line + "\n" + string(existing)
	return os.WriteFile(path, []byte(content), 0o644) //nolint:gosec // passwd and group are world-readable by design
}
