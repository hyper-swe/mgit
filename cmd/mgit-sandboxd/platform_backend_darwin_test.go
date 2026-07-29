//go:build darwin && (vzf || !cgo)

package main

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewHypervisorBackend_Vzf_LogsWhichVMMLinked verifies the vzf wiring
// logs the linked VMM before attempting to construct the manager. This file
// only compiles for a build that deliberately opts OUT of the darwin
// default (-tags vzf, or CGO disabled) since ADR-010's GA position made
// libkrun the darwin default — the log line still must fire so an operator
// can tell such a build chose vzf rather than assuming libkrun. Refs: ADR-010
func TestNewHypervisorBackend_Vzf_LogsWhichVMMLinked(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	clock := func() time.Time { return time.Unix(0, 0).UTC() }

	_, _, _ = newHypervisorBackend(hypervisorDeps{
		repoRoot: t.TempDir(),
		workDir:  t.TempDir(),
		logger:   logger,
		clock:    clock,
	})

	out := buf.String()
	assert.Contains(t, out, "vmm_linked", "must log which VMM was linked at build time")
	assert.Contains(t, out, "vzf", "this build was made with -tags vzf (or CGO disabled)")
}
