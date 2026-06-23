//go:build linux

package firecracker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// TestFcVM_PeerIdentity_PerVMDistinct_NotConstantCID is the F-E regression at
// the source: a Firecracker VM's host-observed peer identity must be PER-VM
// distinct, never the constant "cid:3" (every guest shares guestVsockCID, so
// keying authorization on it would let any guest masquerade as any other). The
// identity is the per-VM vsock socket path. Two VMs with different state dirs
// must report different identities. Refs: MGIT-11.10.11, SEC-10
func TestFcVM_PeerIdentity_PerVMDistinct_NotConstantCID(t *testing.T) {
	vmA := &fcVM{vsockPath: sandboxPaths(microvm.SandboxStateDir("/work", "sbx-A")).vsock}
	vmB := &fcVM{vsockPath: sandboxPaths(microvm.SandboxStateDir("/work", "sbx-B")).vsock}

	idA := vmA.PeerIdentity()
	idB := vmB.PeerIdentity()

	assert.NotEqual(t, "cid:3", idA, "the per-VM identity must not be the shared guest CID (F-E)")
	assert.NotEqual(t, idA, idB, "two VMs report distinct host-observed identities")
	assert.Equal(t, vmA.vsockPath, idA, "the identity is the per-VM vsock socket path")
}

// TestFcVM_NotifySocketPath_DerivesReverseVsock proves the VM exposes the
// per-VM host notify socket (the firecracker reverse-vsock "<vsock>_<port>")
// the guest reaches the host on, and an unset vsock path yields no path (the
// auto-land trigger is then disabled). Refs: MGIT-11.10.11
func TestFcVM_NotifySocketPath_DerivesReverseVsock(t *testing.T) {
	vsockPath := sandboxPaths(microvm.SandboxStateDir("/work", "sbx-A")).vsock
	vm := &fcVM{vsockPath: vsockPath}
	assert.Equal(t, reverseVsockSocketPath(vsockPath, microvm.GuestNotifyPort), vm.NotifySocketPath())

	empty := &fcVM{}
	assert.Empty(t, empty.NotifySocketPath(), "no vsock path -> no notify socket")
}
