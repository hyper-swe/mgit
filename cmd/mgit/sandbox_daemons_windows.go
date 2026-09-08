//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// The sandbox daemon does not run on Windows in v1 (the Hyper-V backend
// arrives with MGIT-11.5.3), so no daemon record can exist here. These are
// what the daemon verbs answer with on Windows — a stated refusal, never a
// silent success — instead of a build that does not compile: syscall.Kill in
// an untagged file broke both Windows targets of mgit (MGIT-198).

// hostKill refuses: signaling a daemon by pid is a unix notion, and there is
// no sandbox daemon here to signal. Refs: MGIT-198, MGIT-11.5.3
func hostKill(pid int, _ syscall.Signal) error {
	return fmt.Errorf("stop pid %d: the sandbox daemon does not run on Windows in v1 "+
		"(the Hyper-V backend arrives with MGIT-11.5.3), so there is nothing to signal", pid)
}

// pidAlive answers whether a process exists through the handle Windows opens
// for it; there is no signal 0 here. Refs: MGIT-198
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
