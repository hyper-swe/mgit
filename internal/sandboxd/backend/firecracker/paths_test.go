package firecracker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// TestReverseVsockSocketPath_PerVMDistinct proves the guest->host reverse-vsock
// socket path is the firecracker "<vsock_uds>_<port>" convention and is DISTINCT
// per VM (it embeds the per-VM vsock socket path). This is the host-observed
// per-VM identity the notify listener authorizes on (F-E). Refs: MGIT-11.10.11, SEC-10
func TestReverseVsockSocketPath_PerVMDistinct(t *testing.T) {
	a := sandboxPaths(microvm.SandboxStateDir("/work", "sbx-A")).vsock
	b := sandboxPaths(microvm.SandboxStateDir("/work", "sbx-B")).vsock

	notifyA := reverseVsockSocketPath(a, microvm.GuestNotifyPort)
	notifyB := reverseVsockSocketPath(b, microvm.GuestNotifyPort)

	assert.Equal(t, a+"_1026", notifyA, "host listens on <vsock>_<notifyport>")
	assert.NotEqual(t, notifyA, notifyB, "the notify socket is per-VM distinct (F-E)")
}
