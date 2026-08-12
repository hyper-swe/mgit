//go:build linux

// Parent-death wiring for the firecracker VMM (MGIT-103). Pure construction:
// no /dev/kvm, no firecracker binary, no guest image — so the wiring that
// stops a SIGKILLed daemon orphaning its microVM is asserted on every Linux
// run, not only on the KVM gate. The DELIVERY of the signal is what the KVM
// gate and scripts/e2e/sandbox_registry_durability.sh prove live.
// Refs: FR-17.19, MGIT-103
package firecracker

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestTieVMMToParent_SetsPdeathsigKill pins the mechanism: the kernel must
// SIGKILL the VMM when the thread that forked it dies, because an ungraceful
// daemon exit (SIGKILL, OOM kill, an escaped panic) never runs the drain that
// would otherwise stop the VM. Refs: FR-17.19, MGIT-103
func TestTieVMMToParent_SetsPdeathsigKill(t *testing.T) {
	tests := []struct {
		name string
		cmd  *exec.Cmd
	}{
		{
			name: "no_existing_attributes",
			cmd:  &exec.Cmd{Path: "/usr/bin/firecracker"},
		},
		{
			// Must not clobber whatever the SDK (or a future caller) already
			// set — the death signal is an addition, not a replacement.
			name: "preserves_existing_attributes",
			cmd: &exec.Cmd{
				Path:        "/usr/bin/firecracker",
				SysProcAttr: &syscall.SysProcAttr{Setsid: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existingSetsid := tt.cmd.SysProcAttr != nil && tt.cmd.SysProcAttr.Setsid

			tieVMMToParent(tt.cmd)

			if tt.cmd.SysProcAttr == nil {
				t.Fatal("SysProcAttr is nil; the VMM would outlive its daemon")
			}
			if tt.cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
				t.Errorf("Pdeathsig = %v, want SIGKILL", tt.cmd.SysProcAttr.Pdeathsig)
			}
			if tt.cmd.SysProcAttr.Setsid != existingSetsid {
				t.Error("tieVMMToParent clobbered an attribute it does not own")
			}
			// Setpgid is deliberately NOT set: leaving the VMM in the daemon's
			// process group keeps a group-directed signal reaching it, which
			// its own group would take away for nothing.
			if tt.cmd.SysProcAttr.Setpgid {
				t.Error("Setpgid set: the VMM would escape a group-directed signal")
			}
		})
	}
}

// TestFCVM_Teardown_ReleasesThePinnedThread covers the ordering the mechanism
// depends on in both directions: the pinned forking thread is held while the
// VM runs, and released at teardown (its exit is itself a SIGKILL to the VMM,
// so it must not come earlier). Refs: MGIT-103
func TestFCVM_Teardown_ReleasesThePinnedThread(t *testing.T) {
	released := make(chan struct{})
	v := &fcVM{
		cancel:  func() {},
		console: devNull(t),
		unpin:   func() { close(released) },
	}

	select {
	case <-released:
		t.Fatal("the forking thread was released before teardown; a live VM would be killed")
	default:
	}

	v.teardown()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not release the pinned thread; one OS thread leaks per VM")
	}

	// teardown runs from Start's failure path AND from Stop; a second release
	// must not panic on an already-closed channel.
	v.teardown()
}

// devNull opens a throwaway console handle for the teardown test.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	return f
}
