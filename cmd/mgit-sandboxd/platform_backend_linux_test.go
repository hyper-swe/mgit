//go:build linux && !(libkrun && cgo)

package main

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewHypervisorBackend_LogsWhichVMMLinked verifies the Linux wiring logs
// the linked VMM (firecracker, the GA default) before attempting to
// construct the manager, so an operator can always tell which backend a
// given daemon build actually linked even when construction itself fails
// (e.g. no firecracker binary or /dev/kvm in this environment). Mirrors the
// libkrun wiring's existing "vmm_linked" log line. Refs: ADR-010
func TestNewHypervisorBackend_LogsWhichVMMLinked(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	// newStoreProvisioner only needs a resolvable repo root; construction
	// of the underlying firecracker manager may still fail in this test
	// environment (no /dev/kvm or firecracker binary) — that is fine, the
	// log line must appear regardless, before that failure.
	_, _, _ = newHypervisorBackend(hypervisorDeps{
		repoRoot: t.TempDir(),
		workDir:  t.TempDir(),
		logger:   logger,
		clock:    clock,
	})

	out := buf.String()
	assert.Contains(t, out, "vmm_linked", "must log which VMM was linked at build time")
	assert.Contains(t, out, "kvm", "the Linux GA default backend is firecracker/kvm")
}
