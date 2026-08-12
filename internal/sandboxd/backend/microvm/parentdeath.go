package microvm

import (
	"runtime"
	"sync"
)

// ForkPinned runs fork on an OS thread dedicated to it, and keeps that thread
// alive until the returned release is called. It exists for one reason: Linux
// delivers a child's parent-death signal (PR_SET_PDEATHSIG, the mechanism that
// stops a SIGKILLed daemon orphaning its microVM) when the FORKING THREAD
// exits — not when the parent process exits.
//
// That distinction is a real footgun in Go. The runtime schedules goroutines
// onto whatever thread is free, and it terminates a thread whose locked
// goroutine exits, so a VMM forked from an ordinary goroutine can be SIGKILLed
// by a thread that merely finished its work while the daemon and the VM are
// both perfectly healthy — a rare, unreproducible "my sandbox vanished".
// Every mitigation short of owning the thread for the child's lifetime narrows
// that window rather than closing it, which is why this holds the thread
// outright.
//
// The cost is one parked OS thread per running VM, bounded by the fleet
// ceiling (FR-17.26); the pinning goroutine exits — deliberately still locked,
// which is what ends the thread — when release is called at teardown.
//
// release is never nil and is safe to call more than once: the Start failure
// path and Stop both reach it. A fork that FAILS releases its own thread
// before returning, since its caller tears down rather than holding a handle.
//
// Refs: FR-17.19, FR-17.16, MGIT-103
func ForkPinned(fork func() error) (release func(), err error) {
	release, _, err = forkPinned(fork)
	return release, err
}

// forkPinned is ForkPinned plus a channel closed once the pinning goroutine
// (and with it its OS thread) has exited, so the lifetime it guarantees can be
// asserted directly rather than inferred from goroutine counts.
func forkPinned(fork func() error) (release func(), exited <-chan struct{}, err error) {
	forked := make(chan error, 1)
	released := make(chan struct{})
	gone := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }

	go func() {
		defer close(gone)
		runtime.LockOSThread()
		// Deliberately NOT unlocked. A goroutine that exits while locked
		// terminates its OS thread, and that termination is exactly what the
		// child's parent-death signal keys on — so it must happen only after
		// release, by which time the child is being torn down anyway.
		forked <- fork()
		<-released
	}()

	if forkErr := <-forked; forkErr != nil {
		release()
		return release, gone, forkErr
	}
	return release, gone, nil
}
