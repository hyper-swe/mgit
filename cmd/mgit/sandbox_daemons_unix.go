//go:build unix

package main

import (
	"errors"
	"syscall"
)

// hostKill signals a pid — the stop verb's signal, behind the identity check
// that precedes it. Refs: MGIT-191
func hostKill(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// pidAlive asks the kernel, not ps: signal 0 probes without touching. A pid
// that exists but belongs to another user answers EPERM, which is still
// "alive". Refs: MGIT-191
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
