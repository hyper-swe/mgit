package libkrun

import (
	"context"
	"io/fs"
	"os/exec"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// abortedExit is a real *exec.ExitError for a child killed by SIGABRT, the way
// a VMM that aborts at startup is reaped.
func abortedExit(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "kill -ABRT $$").Run()
	if err == nil {
		t.Fatal("the helper child should have been killed by a signal")
	}
	return err
}

// A boot that fails because the host has no KVM device must say so outright,
// and a VMM that dies by signal must name the signal: the console tail alone
// lost both (the cause was a libkrun abort, signal 6, exit status 2 on a host
// with no /dev/kvm). Refs: MGIT-302
func TestBootFailureCause(t *testing.T) {
	noDev := &fs.PathError{Op: "open", Path: "/dev/kvm", Err: fs.ErrNotExist}
	t.Run("a_missing_kvm_device_is_named_outright", func(t *testing.T) {
		got := bootFailureCause(noDev, nil)
		for _, w := range []string{"no KVM device", "/dev/kvm"} {
			if !strings.Contains(got, w) {
				t.Errorf("cause = %q, want it to contain %q", got, w)
			}
		}
	})
	t.Run("a_vmm_killed_by_a_signal_names_the_signal", func(t *testing.T) {
		got := bootFailureCause(nil, abortedExit(t))
		if !strings.Contains(got, "signal: ") {
			t.Errorf("cause = %q, want the signal named (its wording differs by OS)", got)
		}
	})
	t.Run("nothing_known_is_empty_never_a_guess", func(t *testing.T) {
		if got := bootFailureCause(nil, nil); got != "" {
			t.Errorf("cause = %q, want empty", got)
		}
	})
}

func TestKrunVM_Start_ChildDiesWithNoKVMDevice_SaysSo(t *testing.T) {
	child := newFakeChild("") // EOF before any handshake line
	child.exitCode = 2
	child.waitErr = abortedExit(t)
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)
	h.kvmProbe = func() error { return &fs.PathError{Op: "open", Path: "/dev/kvm", Err: fs.ErrNotExist} }

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	err = vm.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded")
	}
	for _, w := range []string{"no KVM device", "signal: ", "console"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("Start error = %q, want it to contain %q", err, w)
		}
	}
}

// A child that reported its own error and was then killed by mgit must not be
// blamed on a signal: the status reaped after mgit's SIGKILL is mgit's.
func TestKrunVM_Start_ReportedErrorThenOurKill_NamesNoSignal(t *testing.T) {
	child := newFakeChild(`{"ok":false,"error":"krun_add_net_unixgram: -22"}` + "\n")
	child.exitCode = -1
	child.waitErr = abortedExit(t) // what Wait returns after Process.Kill
	spawner := &fakeSpawner{next: func() *fakeChild { return child }}
	h := testHypervisor(t, spawner)
	h.kvmProbe = func() error { return nil }

	vm, err := h.CreateVM(vmCfg(t, model.NetworkModeNone))
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	err = vm.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "krun_add_net_unixgram") {
		t.Fatalf("Start = %v, want the child's own error", err)
	}
	if strings.Contains(err.Error(), "ended with") {
		t.Errorf("Start error = %q, must not name a signal mgit itself sent", err)
	}
}
