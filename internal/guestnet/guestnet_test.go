package guestnet

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guestboot"
)

// fakeLink records what the boot path asked the kernel to do.
type fakeLink struct {
	calls []guestboot.GuestNetwork
	ifs   []string
	err   error
}

func (f *fakeLink) Configure(iface string, n guestboot.GuestNetwork) error {
	f.ifs = append(f.ifs, iface)
	f.calls = append(f.calls, n)
	return f.err
}

// testDeps wires a fake link and a temp resolv.conf, so every assertion runs
// without privilege on any platform.
func testDeps(t *testing.T, link Linker) (Deps, string) {
	t.Helper()
	resolv := filepath.Join(t.TempDir(), "etc", "resolv.conf")
	return Deps{
		Link:        link,
		ResolvPath:  resolv,
		LookupIface: func(string) error { return nil },
		Sleep:       func(time.Duration) {},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, resolv
}

var validNet = guestboot.GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2", DNS: "10.0.2.2"}

// TestApply_ValidDescriptor_ConfiguresEth0AndResolver is the fix for MGIT-68
// in one assertion: given the host's descriptor, the guest addresses eth0 and
// points its resolver at the gateway. Refs: MGIT-68, FR-17.7, SEC-07
func TestApply_ValidDescriptor_ConfiguresEth0AndResolver(t *testing.T) {
	link := &fakeLink{}
	deps, resolv := testDeps(t, link)

	require.NoError(t, Apply(validNet, deps))

	require.Len(t, link.calls, 1)
	assert.Equal(t, validNet, link.calls[0])
	assert.Equal(t, []string{NIC}, link.ifs, "the NIC must be selected by name")

	data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "nameserver 10.0.2.2\n", string(data))
}

// TestApply_NoDNSToken_UsesGatewayAsResolver verifies an omitted resolver
// falls back to the gateway, where the host-side pinning resolver listens.
// Without this a guest has a route but still fails every name with EAI_AGAIN
// — half the reported MGIT-68 symptom. Refs: SEC-07, MGIT-68
func TestApply_NoDNSToken_UsesGatewayAsResolver(t *testing.T) {
	deps, resolv := testDeps(t, &fakeLink{})
	n := guestboot.GuestNetwork{IP: "10.0.2.15", PrefixLen: 24, Gateway: "10.0.2.2"}

	require.NoError(t, Apply(n, deps))

	data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "nameserver 10.0.2.2\n", string(data))
}

// TestApply_NoDescriptor_IsANoOp verifies a none-mode sandbox (no network
// tokens at all) configures nothing and does not fail the boot.
func TestApply_NoDescriptor_IsANoOp(t *testing.T) {
	link := &fakeLink{}
	deps, resolv := testDeps(t, link)

	require.NoError(t, Apply(guestboot.GuestNetwork{}, deps))

	assert.Empty(t, link.calls, "no descriptor means no NIC configuration")
	_, err := os.Stat(resolv)
	assert.True(t, os.IsNotExist(err), "a network-less sandbox gets no resolver either")
}

// TestApply_PartialDescriptor_FailsClosed verifies a half-specified
// descriptor aborts the boot with a reason instead of configuring a NIC from
// part of one. Refs: MGIT-68
func TestApply_PartialDescriptor_FailsClosed(t *testing.T) {
	tests := map[string]guestboot.GuestNetwork{
		"no_gateway": {IP: "10.0.2.15", PrefixLen: 24},
		"no_prefix":  {IP: "10.0.2.15", Gateway: "10.0.2.2"},
		"bad_ip":     {IP: "10.0.2.999", PrefixLen: 24, Gateway: "10.0.2.2"},
	}
	for name, n := range tests {
		t.Run(name, func(t *testing.T) {
			link := &fakeLink{}
			deps, _ := testDeps(t, link)

			err := Apply(n, deps)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "incomplete")
			assert.Empty(t, link.calls, "nothing may be configured from a partial descriptor")
		})
	}
}

// TestApply_LinkFailure_IsReported verifies an ioctl failure surfaces rather
// than leaving a silently unaddressed NIC — the exact condition that shipped.
func TestApply_LinkFailure_IsReported(t *testing.T) {
	link := &fakeLink{err: errors.New("set addr: operation not permitted")}
	deps, _ := testDeps(t, link)

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation not permitted")
	assert.Contains(t, err.Error(), NIC)
}

// TestApply_NICAppearsLate_IsWaitedFor verifies a NIC the kernel has not
// enumerated yet is waited for rather than failing the boot on the first look.
func TestApply_NICAppearsLate_IsWaitedFor(t *testing.T) {
	link := &fakeLink{}
	deps, _ := testDeps(t, link)
	attempts := 0
	deps.LookupIface = func(string) error {
		attempts++
		if attempts < 3 {
			return errors.New("no such network interface")
		}
		return nil
	}
	slept := 0
	deps.Sleep = func(time.Duration) { slept++ }

	require.NoError(t, Apply(validNet, deps))
	assert.Equal(t, 3, attempts)
	assert.Equal(t, 2, slept)
	assert.Len(t, link.calls, 1)
}

// TestApply_NICNeverAppears_FailsWithReason verifies the wait is bounded and
// the failure names the interface, rather than hanging boot forever.
func TestApply_NICNeverAppears_FailsWithReason(t *testing.T) {
	link := &fakeLink{}
	deps, _ := testDeps(t, link)
	deps.LookupIface = func(string) error { return errors.New("no such network interface") }

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), NIC)
	assert.Empty(t, link.calls)
}

// TestApply_ResolvConfIsReplaced verifies the base image's own resolv.conf is
// overwritten rather than appended to, and that a SYMLINK (what a stock image
// often ships, frequently dangling) is replaced by a real file instead of
// being followed out of the guest's control.
func TestApply_ResolvConfIsReplaced(t *testing.T) {
	deps, resolv := testDeps(t, &fakeLink{})
	require.NoError(t, os.MkdirAll(filepath.Dir(resolv), 0o750))

	t.Run("existing_file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(resolv, []byte("nameserver 8.8.8.8\n"), 0o644)) //nolint:gosec // test fixture

		require.NoError(t, Apply(validNet, deps))

		data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
		require.NoError(t, err)
		assert.Equal(t, "nameserver 10.0.2.2\n", string(data))
	})

	t.Run("dangling_symlink", func(t *testing.T) {
		require.NoError(t, os.Remove(resolv))
		require.NoError(t, os.Symlink("/run/systemd/resolve/stub-resolv.conf", resolv))

		require.NoError(t, Apply(validNet, deps))

		info, err := os.Lstat(resolv)
		require.NoError(t, err)
		assert.Zero(t, info.Mode()&os.ModeSymlink, "the symlink must be replaced by a real file")
		data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
		require.NoError(t, err)
		assert.Equal(t, "nameserver 10.0.2.2\n", string(data))
	})
}

// TestApply_ResolvWriteFailure_IsReported verifies an unwritable resolver
// path fails the boot: a guest with a route and no resolver resolves nothing.
func TestApply_ResolvWriteFailure_IsReported(t *testing.T) {
	deps, _ := testDeps(t, &fakeLink{})
	// A path whose parent is a FILE cannot be created.
	blocker := filepath.Join(t.TempDir(), "etc")
	require.NoError(t, os.WriteFile(blocker, []byte("not a dir"), 0o600))
	deps.ResolvPath = filepath.Join(blocker, "resolv.conf")

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolv.conf")
}

// TestApply_ResolvPathIsADirectory_IsReported verifies a resolver path that
// cannot be replaced (here a non-empty directory) fails the boot with the path
// named, rather than leaving the guest resolving nothing.
func TestApply_ResolvPathIsADirectory_IsReported(t *testing.T) {
	deps, resolv := testDeps(t, &fakeLink{})
	require.NoError(t, os.MkdirAll(filepath.Join(resolv, "occupied"), 0o750))

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "replace")
	assert.Contains(t, err.Error(), "resolv.conf")
}

// TestDeps_WithDefaults verifies the optional collaborators are filled, so a
// caller that supplies only a Linker still gets a working boot path (and no
// nil-pointer dereference in PID 1, where one is fatal to the sandbox).
func TestDeps_WithDefaults(t *testing.T) {
	d := Deps{Link: &fakeLink{}}.withDefaults()
	assert.NotNil(t, d.Logger)
	assert.NotNil(t, d.Sleep)
	assert.Equal(t, DefaultResolvPath, d.ResolvPath)

	// The default interface lookup must be a REAL lookup: loopback exists on
	// every machine ("lo" on Linux, "lo0" on darwin), and a name nothing can
	// have does not.
	require.NotNil(t, d.LookupIface)
	assert.True(t, d.LookupIface("lo") == nil || d.LookupIface("lo0") == nil,
		"loopback must resolve under one of its names")
	assert.Error(t, d.LookupIface("mgit-no-such-nic0"))
}

// TestApply_NoLinker_FailsClosed verifies a build with no link implementation
// (a non-Linux guest) reports that rather than pretending to configure a NIC.
func TestApply_NoLinker_FailsClosed(t *testing.T) {
	deps, _ := testDeps(t, nil)
	deps.Link = nil

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no link configurator")
}

// TestApply_ResolverOnly_WritesResolvConfAndLeavesTheLinkAlone is the MGIT-69
// guest half: firecracker's guest is already addressed by the kernel's `ip=`
// autoconfiguration, so mgit-guest must write the resolver and touch nothing
// else. Re-applying an address there would duplicate a working mechanism, and
// the NIC wait would be dead weight. Refs: MGIT-69
func TestApply_ResolverOnly_WritesResolvConfAndLeavesTheLinkAlone(t *testing.T) {
	link := &fakeLink{}
	deps, resolv := testDeps(t, link)
	looked := 0
	deps.LookupIface = func(string) error { looked++; return nil }

	require.NoError(t, Apply(guestboot.GuestNetwork{DNS: "172.31.4.1"}, deps))

	assert.Empty(t, link.calls, "resolver-only must not configure the link")
	assert.Zero(t, looked, "resolver-only must not wait for the NIC")
	data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "nameserver 172.31.4.1\n", string(data))
}

// TestApply_ResolverOnly_NeedsNoLinker verifies the resolver-only path does
// not fail closed on a nil Linker: there is no link to configure, so a
// missing configurator is irrelevant rather than fatal.
func TestApply_ResolverOnly_NeedsNoLinker(t *testing.T) {
	deps, resolv := testDeps(t, nil)
	deps.Link = nil

	require.NoError(t, Apply(guestboot.GuestNetwork{DNS: "172.31.4.1"}, deps))

	data, err := os.ReadFile(resolv) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	assert.Equal(t, "nameserver 172.31.4.1\n", string(data))
}

// TestApply_LinkDescriptor_StillNeedsALinker keeps the fail-closed behavior
// where it belongs: a descriptor that DOES ask for link configuration must
// not silently succeed without a configurator.
func TestApply_LinkDescriptor_StillNeedsALinker(t *testing.T) {
	deps, _ := testDeps(t, nil)
	deps.Link = nil

	err := Apply(validNet, deps)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no link configurator")
}
