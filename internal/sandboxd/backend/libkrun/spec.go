package libkrun

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
)

// vmSpec is the complete, serializable description of one libkrun microVM.
//
// It exists because libkrun is one-VM-per-PROCESS (krun_start_enter never
// returns and exit()s with the guest's exit code — ADR-010), so the daemon
// re-execs itself and the VM's configuration must cross that process
// boundary. It crosses on the child's STDIN as one JSON document — never
// argv, which is world-readable via ps. Refs: ADR-010, MGIT-61.8
type vmSpec struct {
	SandboxID string `json:"sandbox_id"`
	// TaskID identifies the task for the child's egress audit records.
	TaskID   string `json:"task_id,omitempty"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
	// StateDir is the per-sandbox state directory; every per-VM host artifact
	// (net backing sockets, vsock sockets) lives under it so teardown stays
	// one RemoveAll (FR-17.19).
	StateDir string `json:"state_dir"`
	// RootDir is the guest's root filesystem: a host DIRECTORY shared over
	// virtiofs (libkrunfw brings the kernel), not a disk image.
	RootDir string `json:"root_dir"`
	// RootReadOnly exposes the root share read-only (FR-17.17 immutable image).
	RootReadOnly bool `json:"root_read_only"`
	// WorktreeHostDir is the HOST directory shared into the guest as a second
	// virtiofs device tagged WorktreeTag. It is deliberately distinct from
	// WorktreePath: under SEC-03 the shared source is a STAGED copy under the
	// sandbox state dir (worktree files + the private .mgit, host store
	// excluded), not the live worktree — a live share cannot exclude or rebind
	// host-side. Empty when no worktree is delivered. Refs: SEC-03, FR-17.3
	WorktreeHostDir string `json:"worktree_host_dir,omitempty"`
	// WorktreePath is the IDENTICAL absolute path the guest mounts the share
	// at, and the workload's working directory (FR-17.3). The guest learns to
	// mount it from the boot tokens carried in ExecEnv.
	WorktreePath string `json:"worktree_path,omitempty"`
	WorktreeTag  string `json:"worktree_tag,omitempty"`
	// VsockEnabled wires the control-plane vsock ports (exec/land/notify)
	// to per-VM unix socket paths under StateDir.
	VsockEnabled bool `json:"vsock_enabled"`
	// PublishPorts are the GUEST TCP ports exposed for one-way host->guest
	// publishing (SEC-09). Each becomes a LISTENING libkrun vsock port: the
	// VMM listens on a host unix socket under StateDir and forwards inbound
	// connections to that guest vsock port, where mgit-guest bridges them to
	// the guest's own loopback. Host-initiated only — no path back out.
	// Refs: SEC-09, FR-17.8
	PublishPorts []int `json:"publish_ports,omitempty"`
	// ExecPath is the guest PID-1 workload, guest-root-relative. ExecArgs are
	// its ARGUMENTS ONLY: libkrun prepends the executable to argv itself, so
	// including it here would shift every guest arg by one (ADR-010).
	ExecPath string   `json:"exec_path"`
	ExecArgs []string `json:"exec_args,omitempty"`
	// ExecEnv is the guest environment, passed EXPLICITLY even when empty:
	// libkrun's NULL-envp convenience would inject the daemon's own
	// environment into the guest (SEC-05 host-state leak).
	ExecEnv []string `json:"exec_env,omitempty"`
	// NetworkMode + Allowlist carry the egress policy the child enforces in
	// its own netstack gateway (SEC-04). The allowlist entries are the
	// launch policy's, verbatim.
	NetworkMode string   `json:"network_mode"`
	Allowlist   []string `json:"allowlist,omitempty"`
}

// Validate rejects a spec that could not configure a VM, before any process
// is spawned or resource bound. RootDir must be an existing directory: a
// disk-image path here means the caller is holding a firecracker/vzf-style
// bundle, which libkrun cannot boot — say so instead of failing in the child.
func (s vmSpec) Validate() error {
	switch {
	case s.SandboxID == "":
		return fmt.Errorf("libkrun spec: sandbox id must not be empty")
	case s.StateDir == "":
		return fmt.Errorf("libkrun spec: state dir must not be empty")
	case s.RootDir == "":
		return fmt.Errorf("libkrun spec: guest root dir must not be empty")
	case s.ExecPath == "":
		return fmt.Errorf("libkrun spec: guest exec path must not be empty")
	}
	info, err := os.Stat(s.RootDir)
	if err != nil {
		return fmt.Errorf("libkrun spec: guest root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf(
			"libkrun spec: guest root %s is a file, not a directory: libkrun shares "+
				"the guest root over virtiofs and cannot boot a disk image", s.RootDir)
	}
	if _, err := netBackingFor(s.SandboxID, s.NetworkMode, s.StateDir); err != nil {
		return err
	}
	if s.VsockEnabled {
		for _, port := range controlVsockPorts() {
			if err := checkSocketPathLen("vsock socket", vsockSocketPath(s.StateDir, port)); err != nil {
				return err
			}
		}
	}
	return nil
}

// encode writes the spec as one JSON document.
func (s vmSpec) encode(w io.Writer) error {
	if err := json.NewEncoder(w).Encode(s); err != nil {
		return fmt.Errorf("encode libkrun vm spec: %w", err)
	}
	return nil
}

// decodeSpec reads one spec from the child's stdin and re-validates it: the
// child is the process that acts on the spec, so it must not trust that the
// parent validated (defense in depth across the exec boundary).
func decodeSpec(r io.Reader) (vmSpec, error) {
	var s vmSpec
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return vmSpec{}, fmt.Errorf("decode libkrun vm spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return vmSpec{}, err
	}
	return s, nil
}

// controlVsockPorts lists the guest control-plane vsock ports the backend
// wires: exec and land (host-initiated), and notify (guest-initiated).
// Refs: FR-17.11, FR-17.5, MGIT-11.10.11
func controlVsockPorts() []uint32 {
	return []uint32{microvm.GuestExecPort, microvm.GuestLandPort, microvm.GuestNotifyPort}
}

// vsockSocketPath is the per-VM unix socket path one guest vsock port maps
// to. For host-initiated ports (exec/land) libkrun listens here and forwards
// to the guest; for the guest-initiated notify port libkrun connects here and
// the daemon listens. Per-VM and under the state dir, so the path itself is
// the host-observed peer identity (SEC-10, same convention as firecracker).
func vsockSocketPath(stateDir string, port uint32) string {
	return filepath.Join(stateDir, fmt.Sprintf("vsock_%d.sock", port))
}
