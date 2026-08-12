package microvm

import (
	"errors"

	"testing"
	"time"
)

// TestForkPinned_ReturnsForkResult_AndHoldsTheThreadUntilRelease pins the
// contract PR_SET_PDEATHSIG depends on: the fork runs on an OS thread that
// exists for the child's whole life, and that thread ends only when the caller
// says so. Refs: MGIT-103
func TestForkPinned_ReturnsForkResult_AndHoldsTheThreadUntilRelease(t *testing.T) {
	forkErr := errors.New("vmm did not start")
	tests := []struct {
		name    string
		fork    func() error
		wantErr error
	}{
		{name: "successful_fork", fork: func() error { return nil }},
		{name: "failed_fork_propagates", fork: func() error { return forkErr }, wantErr: forkErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := make(chan struct{})
			release, err := ForkPinned(func() error {
				close(ran)
				return tt.fork()
			})
			if release == nil {
				t.Fatal("release must never be nil; the caller's teardown would panic")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ForkPinned err = %v, want %v", err, tt.wantErr)
			}
			select {
			case <-ran:
			default:
				t.Fatal("ForkPinned returned before running the fork")
			}
			// Idempotent: teardown paths overlap (Start's failure path and
			// Stop both reach it), and a second close would panic.
			release()
			release()
		})
	}
}

// TestForkPinned_HoldsTheForkingThread_UntilRelease is the property that makes
// the mechanism work: on Linux the child's death signal fires when the FORKING
// THREAD exits, so that thread must outlive the fork. The pinning goroutine is
// what holds it — it must not exit before release, and must exit after.
// Refs: MGIT-103
func TestForkPinned_HoldsTheForkingThread_UntilRelease(t *testing.T) {
	release, exited, err := forkPinned(func() error { return nil })
	if err != nil {
		t.Fatalf("forkPinned: %v", err)
	}
	select {
	case <-exited:
		t.Fatal("the forking thread was released while the child was still running; " +
			"a healthy VM could be SIGKILLed by its own supervision mechanism")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the pinning goroutine outlived release; one OS thread would leak per VM")
	}
}

// TestForkPinned_FailedFork_ReleasesTheThreadItself covers the boundary the
// firecracker Start path relies on: a fork that fails must not leave a pinned
// thread parked forever, because its caller tears down instead of holding a
// release. Refs: MGIT-103
func TestForkPinned_FailedFork_ReleasesTheThreadItself(t *testing.T) {
	_, exited, err := forkPinned(func() error { return errors.New("boom") })
	if err == nil {
		t.Fatal("want the fork error")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("a failed fork left its pinning goroutine parked; the thread leaks")
	}
}
