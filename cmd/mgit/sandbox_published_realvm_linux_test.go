//go:build linux

// Real-VM proof for `mgit sandbox published`, the SEC-09 gap from the
// E2E-MATRIX audit: sandbox_test.go's TestSandboxPublished_ListsPorts only
// ever drove the command against a fake client. This wires it to a REAL
// firecracker-backed model.SandboxManager -- a real KVM microVM, a real
// published port -- through a minimal adapter satisfying this package's
// sandboxClient interface (which is shaped for the daemon RPC, not the raw
// backend manager: Status(taskID) vs the manager's Resolve(sandboxID), for
// instance). Only Launch and Status are real; the other sandboxClient
// methods are unused by `published` and stubbed loudly if ever called.
//
// Network mode is "none" deliberately: SEC-09 port publishing is a vsock
// construct, independent of the tap/iptables NAT modes (allowlist/open),
// which need root and are the owner-pending gap tracked elsewhere. This
// proves the CLI command surfaces real PublishPorts data end to end without
// that dependency. Refs: SEC-09, MGIT-11.10.12, MGIT-61.13
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// realVMAdapter wraps a real firecracker manager to satisfy sandboxClient
// for exactly the one path this test drives (Launch then Status). Every
// other method is unused by `mgit sandbox published` and panics loudly if
// ever called, so a future test that reuses this adapter for a different
// command fails fast rather than silently returning zero values.
type realVMAdapter struct {
	mgr    model.SandboxManager
	byTask map[string]string // taskID -> the manager's own sandbox ID
}

func (a *realVMAdapter) Launch(ctx context.Context, opts model.SandboxLaunchOptions) (*model.SandboxInfo, error) {
	info, err := a.mgr.Launch(ctx, opts)
	if err != nil {
		return nil, err
	}
	a.byTask[opts.TaskID] = info.ID
	return info, nil
}

func (a *realVMAdapter) Status(ctx context.Context, taskID string) (*model.SandboxInfo, error) {
	id, ok := a.byTask[taskID]
	if !ok {
		return nil, fmt.Errorf("no sandbox launched for task %s", taskID)
	}
	return a.mgr.Resolve(ctx, id)
}

func (a *realVMAdapter) List(context.Context) ([]model.SandboxInfo, error) {
	panic("unused by this test")
}
func (a *realVMAdapter) Remove(ctx context.Context, taskID string, force bool) error {
	id := a.byTask[taskID]
	return a.mgr.Remove(ctx, id, force)
}
func (a *realVMAdapter) Exec(context.Context, string, model.ExecRequest, io.Writer, io.Writer) (int, error) {
	panic("unused by this test")
}
func (a *realVMAdapter) Land(context.Context, string) (*controlproto.LandResult, error) {
	panic("unused by this test")
}
func (a *realVMAdapter) Grants(context.Context, string) ([]controlproto.PendingGrant, error) {
	panic("unused by this test")
}
func (a *realVMAdapter) Grant(context.Context, string, string) (*controlproto.GrantResult, error) {
	panic("unused by this test")
}
func (a *realVMAdapter) Shell(context.Context, string, io.Reader, io.Writer, io.Writer) (int, error) {
	panic("unused by this test")
}

// TestSandboxPublished_RealVM_ReportsActualPublishedPort boots a real
// firecracker microVM with a published port and drives the actual `mgit
// sandbox published` cobra command (not the service layer directly) against
// it through realVMAdapter, in both human and --json form.
func TestSandboxPublished_RealVM_ReportsActualPublishedPort(t *testing.T) {
	kernel := os.Getenv("MGIT_TEST_KERNEL")
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set MGIT_TEST_KERNEL and MGIT_E2E_GUEST_ROOTFS to run against a real KVM guest")
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("no usable /dev/kvm: %v", err)
	} else {
		_ = f.Close()
	}

	clock := func() time.Time { return time.Now().UTC() }
	hostRoot := t.TempDir()
	_, err := images.GenerateTrustRoot(context.Background(), hostRoot, noopPublishedAudit{})
	require.NoError(t, err)
	priv, err := images.LoadSigningKey(hostRoot)
	require.NoError(t, err)
	entry, err := images.BuildEntry(kernel, rootfs,
		"console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 random.trust_cpu=on root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest")
	require.NoError(t, err)
	ref, err := images.Register(hostRoot, "mgit-guest", entry, priv)
	require.NoError(t, err)
	store, err := images.NewStore(hostRoot, clock)
	require.NoError(t, err)

	workDir, err := os.MkdirTemp("", "mgpub")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	mgr, err := firecracker.NewManager(firecracker.Config{
		WorkDir: workDir,
		Resolve: func(r string) (firecracker.ImagePaths, error) {
			ri, rerr := store.Resolve(r)
			return firecracker.ImagePaths{KernelPath: ri.KernelPath, RootfsPath: ri.RootfsPath, Cmdline: ri.Cmdline}, rerr
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  clock,
	})
	require.NoError(t, err)

	adapter := &realVMAdapter{mgr: mgr, byTask: map[string]string{}}
	wtPath := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))

	const taskID = "MGIT-PUB-1"
	info, err := adapter.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID: taskID, WorktreePath: wtPath, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone},
		CPUs:    1, MemoryMB: 256,
		PublishPorts: []model.PortPublish{{HostPort: 18080, GuestPort: 3000}},
	})
	require.NoError(t, err, "launch a real microVM with a published port")
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	connect := func(context.Context) (sandboxClient, error) { return adapter, nil }

	t.Run("human", func(t *testing.T) {
		out, err := runSandbox(connect, "published", taskID)
		require.NoError(t, err)
		assert.Contains(t, out, "127.0.0.1:18080 -> guest:3000")
	})
	t.Run("json", func(t *testing.T) {
		out, err := runSandbox(connect, "published", taskID, "--json")
		require.NoError(t, err)
		var got []model.PortPublish
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Equal(t, []model.PortPublish{{HostPort: 18080, GuestPort: 3000}}, got)
	})
}

type noopPublishedAudit struct{}

func (noopPublishedAudit) RecordTrustRootChange(context.Context, string) error { return nil }
