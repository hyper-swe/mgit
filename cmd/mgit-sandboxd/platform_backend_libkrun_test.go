//go:build cgo && !vzf && (darwin || (linux && libkrun))

package main

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// TestNewHypervisorBackend_Libkrun_LogsWhichVMMLinked verifies the libkrun
// wiring logs the linked VMM before attempting to construct the manager, so
// an operator can always tell which backend a given daemon build actually
// linked — this file is now the darwin DEFAULT (ADR-010's GA position), so
// the log line is the only cheap way to confirm which VMM a given binary
// actually chose. Refs: ADR-010
func TestNewHypervisorBackend_Libkrun_LogsWhichVMMLinked(t *testing.T) {
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
	assert.Contains(t, out, "libkrun", "this build links libkrun")
}

// wantLinkedVMM is the VMM the libkrun build (macOS default, Linux with
// -tags libkrun) must report from --vmm, stated here from the build tags
// above rather than read from the code under test.
// Refs: MGIT-229
const wantLinkedVMM = model.BackendLibkrun
