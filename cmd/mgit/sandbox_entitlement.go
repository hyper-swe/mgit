package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// entitlementState is what mgit could determine about the sandbox daemon's
// macOS hypervisor entitlement. entitlementUnknown is a first-class answer:
// "cannot tell" must never be reported as "missing". Refs: MGIT-104
type entitlementState int

const (
	entitlementUnknown entitlementState = iota
	entitlementPresent
	entitlementMissing
)

// hypervisorEntitlementKey is the entitlement libkrun — the macOS GA backend
// (ADR-010) — requires to create a VM. It is a DIFFERENT key from vzf's
// com.apple.security.virtualization; checking the wrong one would clear a
// binary that cannot drive the backend this platform ships.
const hypervisorEntitlementKey = "com.apple.security.hypervisor"

// daemonBinary is the sandbox daemon program whose signing is in question.
const daemonBinary = "mgit-sandboxd"

// entitlementProbeTimeout bounds the codesign call; a diagnostic may never be
// the reason a failing command hangs.
const entitlementProbeTimeout = 3 * time.Second

// probeHypervisorEntitlement reports whether the mgit-sandboxd on this host
// carries the entitlement libkrun needs. Off darwin, or when the daemon or
// codesign cannot be found, the answer is entitlementUnknown and nothing is
// claimed. The check mirrors scripts/e2e/sandbox_posture.sh, which gates the
// live e2e on the same condition.
//
// The binary examined is the one locateSandboxd resolves — beside this mgit,
// then PATH — which is the daemon mgit itself activates, so the verdict is
// about the process that actually failed to start the VM rather than some
// other copy. Refs: MGIT-104, MGIT-64, MGIT-65
func probeHypervisorEntitlement(ctx context.Context) (entitlementState, string) {
	if runtime.GOOS != "darwin" {
		return entitlementUnknown, ""
	}
	path, err := locateSandboxd()
	if err != nil {
		return entitlementUnknown, ""
	}
	ctx, cancel := context.WithTimeout(ctx, entitlementProbeTimeout)
	defer cancel()
	// codesign writes the entitlements plist to stdout and its own complaints
	// (an unsigned binary) to stderr; both are evidence, so both are read.
	//nolint:gosec // fixed program; path is locateSandboxd's resolution of a fixed name
	out, err := exec.CommandContext(ctx, "codesign", "--display", "--entitlements", "-", path).CombinedOutput()
	return entitlementFromCodesign(string(out), err), path
}

// entitlementFromCodesign maps one codesign run to a verdict. A non-zero exit
// with output is a real answer — "code object is not signed at all" is exactly
// the case this diagnoses — while no output at all means codesign never ran,
// which is not evidence of anything. Refs: MGIT-104
func entitlementFromCodesign(output string, err error) entitlementState {
	if strings.Contains(output, hypervisorEntitlementKey) {
		return entitlementPresent
	}
	if err != nil && strings.TrimSpace(output) == "" {
		return entitlementUnknown
	}
	return entitlementMissing
}
