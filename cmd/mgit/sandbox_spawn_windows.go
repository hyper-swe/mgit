//go:build windows

package main

import "os/exec"

// configureDaemonCmd is a no-op on Windows: the v1 microVM sandbox is
// Linux + macOS only (ADR-006), so the daemon is never spawned here. The
// seam exists so the CLI compiles on Windows, where core mgit runs without
// the sandbox. Refs: MGIT-11.10.9, MGIT-12
func configureDaemonCmd(*exec.Cmd) {}
