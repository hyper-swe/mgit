package firecracker

import "path/filepath"

// vmPaths locates every per-VM host artifact under the sandbox state dir
// (the dir that holds the COW overlay). Keeping the API socket, vsock
// socket, and console there makes teardown one RemoveAll with no host
// residue (FR-17.19). The names are the single source of the firecracker
// per-VM layout, shared by the hypervisor (which creates them) and the
// guest dialer (which dials the vsock socket). Refs: FR-17.19
type vmPaths struct{ socket, vsock, console string }

// sandboxPaths returns the per-VM artifact paths under a sandbox state
// dir. The state dir is microvm.Manager's per-sandbox dir
// (<work-dir>/<sandbox-id>), which also holds the COW overlay.
func sandboxPaths(stateDir string) vmPaths {
	return vmPaths{
		socket:  filepath.Join(stateDir, "firecracker.sock"),
		vsock:   filepath.Join(stateDir, "vsock.sock"),
		console: filepath.Join(stateDir, "console.log"),
	}
}
