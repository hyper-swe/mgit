package guest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

func identitySupervisor(t *testing.T) (*Supervisor, string) {
	t.Helper()
	etc := filepath.Join(t.TempDir(), "etc")
	require.NoError(t, os.MkdirAll(etc, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(etc, "group"), []byte("root:x:0:\ndialout:x:20:\n"), 0o600))
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.EtcDir = etc
	return sup, etc
}

func runShell(t *testing.T, sup *Supervisor, req model.ExecRequest) (Outcome, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	out, err := sup.Execute(context.Background(), req, &stdout, &stderr)
	return out, stdout.String() + stderr.String(), err
}

// The guest ALWAYS echoes the identity a command ran as, so a host that
// asked for nothing still learns it ran as the guest's own identity.
// Refs: MGIT-151
func TestExecute_NoRunAs_EchoesTheProcessIdentity(t *testing.T) {
	sup, _ := identitySupervisor(t)
	out, _, err := runShell(t, sup, model.ExecRequest{Command: []string{"sh", "-c", "true"}})
	require.NoError(t, err)
	require.NotNil(t, out.RanAs)
	assert.Equal(t, os.Getuid(), out.RanAs.UID)
	assert.Equal(t, os.Getgid(), out.RanAs.GID)
}

// Asked for the identity the guest already is (the host-side test's own
// uid), the supervisor materializes it — a passwd entry, a group entry, a
// home that exists — gives the child HOME/USER/LOGNAME for it, and echoes
// it. Refs: MGIT-151
func TestExecute_RunAsTheCurrentIdentity_MaterializesAndEchoesIt(t *testing.T) {
	sup, etc := identitySupervisor(t)
	home := filepath.Join(t.TempDir(), "home", "agent")
	id := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: home}
	out, text, err := runShell(t, sup, model.ExecRequest{
		Command: []string{"sh", "-c", `echo "HOME=$HOME USER=$USER LOGNAME=$LOGNAME"; touch "$HOME/.probe"`},
		RunAs:   &id,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode, text)
	assert.Contains(t, text, "HOME="+home+" USER=agent LOGNAME=agent")
	assert.FileExists(t, filepath.Join(home, ".probe"), "the home exists and is writable by the identity")
	require.NotNil(t, out.RanAs)
	assert.Equal(t, id, *out.RanAs)

	passwd, err := os.ReadFile(filepath.Join(etc, "passwd")) //nolint:gosec // test: a scratch etc dir
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(passwd), "agent:x:"), "the identity's passwd line is PREPENDED so its uid resolves to its name: %q", passwd)
	assert.Contains(t, string(passwd), "root:x:0:0:root:/root:/bin/sh", "existing entries are kept")
	group, err := os.ReadFile(filepath.Join(etc, "group")) //nolint:gosec // test: a scratch etc dir
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(group), "agent:x:"), "the group line is prepended so the gid resolves to the identity's name, not a distro group that happens to share the number")

	// A second exec changes nothing: one line each, not one per exec.
	_, _, err = runShell(t, sup, model.ExecRequest{Command: []string{"true"}, RunAs: &id})
	require.NoError(t, err)
	passwd2, _ := os.ReadFile(filepath.Join(etc, "passwd")) //nolint:gosec // test: a scratch etc dir
	assert.Equal(t, 1, strings.Count(string(passwd2), "agent:x:"), "idempotent")
}

// Asked for an identity this process cannot MATERIALIZE (an unprivileged
// process cannot give a home to another uid), the supervisor refuses to
// start the command: it never runs it as itself instead. The credential
// switch itself is proven where a process may make it — the root-gated
// test below, and the live guests. Refs: MGIT-151
func TestExecute_IdentityItCannotMaterialize_FailsClosedBeforeStarting(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can materialize any identity; the refusal needs an unprivileged process")
	}
	sup, _ := identitySupervisor(t)
	marker := filepath.Join(t.TempDir(), "ran")
	id := model.GuestIdentity{UID: os.Getuid() + 1, GID: os.Getgid(), Name: "other", Home: filepath.Join(t.TempDir(), "h")}
	_, _, err := runShell(t, sup, model.ExecRequest{Command: []string{"sh", "-c", "touch " + marker}, RunAs: &id})
	require.Error(t, err)
	assert.ErrorContains(t, err, "guest exec: identity")
	assert.NoFileExists(t, marker, "the command never ran under the wrong identity")
}

// The credential switch, where a process may make it: as root, a command
// asked to run as an unprivileged identity reports that identity's uid from
// inside — `id -u` is the child's own view, not the supervisor's claim —
// and a file it writes into its home is owned by it. This is the guest's
// path exactly; it runs in the e2e root-gated half. Refs: MGIT-151
func TestExecute_AsRoot_SwitchesToTheAskedIdentity(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("the credential switch needs root (the guest agent is PID 1 root; CI's root-gated half runs this)")
	}
	sup, _ := identitySupervisor(t)
	// Go's t.TempDir is root-owned and owner-only under sudo, so the switched
	// child could not even traverse to the home it was given (CI's root half,
	// 2026-09-08: `touch: … Permission denied` after a correct `id -u`). In a
	// guest the home lives under a traversable root; give the fixture the
	// same property, both temp levels.
	base := t.TempDir()
	require.NoError(t, os.Chmod(base, 0o755))               //nolint:gosec // G302: a test scratch dir the switched child must traverse
	require.NoError(t, os.Chmod(filepath.Dir(base), 0o755)) //nolint:gosec // G302: same, one level up
	home := filepath.Join(base, "home", "agent")
	id := model.GuestIdentity{UID: 65534, GID: 65534, Name: "agent", Home: home}
	out, text, err := runShell(t, sup, model.ExecRequest{
		Command: []string{"sh", "-c", `id -u; id -g; touch "$HOME/.mine"; stat -c %u "$HOME/.mine"`},
		RunAs:   &id,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode, text)
	assert.Equal(t, "65534\n65534\n65534\n", text, "the child's own uid, gid, and the owner of what it wrote")
	require.NotNil(t, out.RanAs)
	assert.Equal(t, id, *out.RanAs)
}

// Root asked for explicitly is honored as-is when the process is root, and
// refused (never faked) when it is not. Refs: MGIT-151
func TestExecute_RunAsRoot_IsHonoredOnlyByRoot(t *testing.T) {
	sup, _ := identitySupervisor(t)
	root := model.RootIdentity()
	out, _, err := runShell(t, sup, model.ExecRequest{Command: []string{"true"}, RunAs: &root, AsRoot: true})
	if os.Getuid() == 0 {
		require.NoError(t, err)
		require.NotNil(t, out.RanAs)
		assert.True(t, out.RanAs.IsRoot())
		return
	}
	require.Error(t, err, "an unprivileged guest agent cannot grant root")
}

// The identity's name and home default when the host leaves them empty,
// and the defaults are the guest's convention. Refs: MGIT-151
func TestIdentityDefaults(t *testing.T) {
	id := withIdentityDefaults(model.GuestIdentity{UID: 501, GID: 20})
	assert.Equal(t, "agent", id.Name)
	assert.Equal(t, "/home/agent", id.Home)
	kept := withIdentityDefaults(model.GuestIdentity{UID: 501, GID: 20, Name: "dev", Home: "/srv/dev"})
	assert.Equal(t, "dev", kept.Name)
	assert.Equal(t, "/srv/dev", kept.Home)
	root := withIdentityDefaults(model.GuestIdentity{UID: 0, GID: 0})
	assert.Equal(t, model.RootIdentity(), root, "root's defaults are root's own, not an agent's")
}

// A home whose parent does not exist yet gets a parent any identity can
// traverse: the live libkrun proof found `/home` created root-owned 0750
// under a root supervisor, so the identity could resolve its home and yet
// not write a byte into it. The home itself stays owner-only. Refs: MGIT-151
func TestEnsureIdentity_CreatedParentsAreTraversable_HomeIsOwnerOnly(t *testing.T) {
	sup, _ := identitySupervisor(t)
	base := t.TempDir()
	home := filepath.Join(base, "home", "agent")
	id := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: home}
	_, err := sup.ensureIdentity(id)
	require.NoError(t, err)
	parent, err := os.Stat(filepath.Join(base, "home"))
	require.NoError(t, err)
	assert.NotZero(t, parent.Mode().Perm()&0o001, "the created parent is traversable by others: %o", parent.Mode().Perm())
	assert.NotZero(t, parent.Mode().Perm()&0o010, "and by the group: %o", parent.Mode().Perm())
	h, err := os.Stat(home)
	require.NoError(t, err)
	assert.Zero(t, h.Mode().Perm()&0o007, "the home itself is not world-accessible: %o", h.Mode().Perm())
}

// A guest whose base ships no /etc at all (the minimal firecracker rootfs
// links a handful of busybox applets and nothing else) still gets its
// passwd and group entries: the supervisor creates the directory it writes
// into. Refs: MGIT-151
func TestEnsureIdentity_CreatesTheEtcDirWhenAbsent(t *testing.T) {
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.EtcDir = filepath.Join(t.TempDir(), "not", "yet", "etc")
	id := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: filepath.Join(t.TempDir(), "home", "agent")}
	_, err := sup.ensureIdentity(id)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(sup.EtcDir, "passwd"))
	assert.FileExists(t, filepath.Join(sup.EtcDir, "group"))
}

// A home the guest's root cannot take — Linux/libkrun refuses metadata
// operations outside /tmp and the worktree (MGIT-89), and CI's libkrun leg
// refused every exec with "chown /root: operation not supported" — falls
// back to a home under the fallback root, and the identity the child gets
// (and echoes) names the home it actually has. Refs: MGIT-151, MGIT-89
func TestEnsureIdentity_UncreatableHome_FallsBackUnderTheFallbackRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can create any directory; the refusal needs an unprivileged process")
	}
	sup, _ := identitySupervisor(t)
	sup.FallbackHomeRoot = filepath.Join(t.TempDir(), "tmp-home")
	locked := filepath.Join(t.TempDir(), "locked")
	require.NoError(t, os.MkdirAll(locked, 0o500))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a test scratch dir, restored so t.TempDir can remove it
	id := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: filepath.Join(locked, "home", "agent")}
	got, err := sup.ensureIdentity(id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(sup.FallbackHomeRoot, "agent"), got.Home, "the home moved under the fallback root")
	assert.DirExists(t, got.Home)
}

// A home that already exists with the right owner is left exactly as it is:
// no chown, which the Linux/libkrun overlay refuses even for root's own
// /root. Refs: MGIT-151, MGIT-89
func TestEnsureIdentity_ExistingOwnedHome_IsLeftAlone(t *testing.T) {
	sup, _ := identitySupervisor(t)
	home := filepath.Join(t.TempDir(), "home", "agent")
	require.NoError(t, os.MkdirAll(home, 0o750))
	before, err := os.Stat(home)
	require.NoError(t, err)
	id := model.GuestIdentity{UID: os.Getuid(), GID: os.Getgid(), Name: "agent", Home: home}
	got, err := sup.ensureIdentity(id)
	require.NoError(t, err)
	assert.Equal(t, home, got.Home)
	after, err := os.Stat(home)
	require.NoError(t, err)
	assert.Equal(t, before.Mode(), after.Mode(), "mode untouched")
}
