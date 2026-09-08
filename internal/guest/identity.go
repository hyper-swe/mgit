package guest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hyper-swe/mgit/internal/model"
)

// Defaults for an identity the host named only by number: the guest's own
// convention for the unprivileged user commands run as. Refs: MGIT-151
const (
	defaultIdentityName = "agent"
	defaultIdentityHome = "/home/agent"
	defaultEtcDir       = "/etc"
)

// withIdentityDefaults fills the name and home the host left empty.
func withIdentityDefaults(id model.GuestIdentity) model.GuestIdentity {
	if id.Name == "" {
		id.Name = defaultIdentityName
	}
	if id.Home == "" {
		id.Home = defaultIdentityHome
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
func (s *Supervisor) ensureIdentity(id model.GuestIdentity) error {
	if id.IsRoot() {
		return nil
	}
	etc := s.EtcDir
	if etc == "" {
		etc = defaultEtcDir
	}
	passwdLine := fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh", id.Name, id.UID, id.GID, id.Home)
	if err := prependEntryUnlessName(filepath.Join(etc, "passwd"), id.Name, passwdLine); err != nil {
		return fmt.Errorf("passwd entry for uid %d: %w", id.UID, err)
	}
	groupLine := fmt.Sprintf("%s:x:%d:", id.Name, id.GID)
	if err := prependEntryUnlessName(filepath.Join(etc, "group"), id.Name, groupLine); err != nil {
		return fmt.Errorf("group entry for gid %d: %w", id.GID, err)
	}
	if err := os.MkdirAll(id.Home, 0o750); err != nil {
		return fmt.Errorf("home %s: %w", id.Home, err)
	}
	if err := os.Chown(id.Home, id.UID, id.GID); err != nil {
		return fmt.Errorf("own home %s: %w", id.Home, err)
	}
	return nil
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

// credentialFor returns the credential a child must be started with to run
// as id, or nil when the supervisor already is that identity. A switch this
// process cannot make fails at start (EPERM) — the child never runs as the
// supervisor instead. Refs: MGIT-151
func credentialFor(id model.GuestIdentity) *syscall.Credential {
	if id.UID == os.Getuid() && id.GID == os.Getgid() {
		return nil
	}
	return &syscall.Credential{Uid: uint32(id.UID), Gid: uint32(id.GID), NoSetGroups: true} //nolint:gosec // ids are validated non-negative by the model
}

// identityEnv is the environment the identity implies for the child.
func identityEnv(id model.GuestIdentity) []string {
	return []string{"HOME=" + id.Home, "USER=" + id.Name, "LOGNAME=" + id.Name}
}
