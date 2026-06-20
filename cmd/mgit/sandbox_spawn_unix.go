//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureDaemonCmd detaches the spawned daemon into its own session so
// it survives the short-lived CLI process (and is not killed by a SIGHUP
// when the CLI's terminal closes). Refs: NFR-17.6, MGIT-11.10.9
func configureDaemonCmd(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
