// Package e2e: the process-lock wait is configurable, as promised.
//
// REQUIREMENTS.md (FR-4.7, NFR-3.5) has always said the lock timeout is
// "configurable via locks.timeout_seconds", and it was not: the wait was a
// compile-time constant, so an operator meeting a contended repo had no knob at
// all. This drives the real binary against a really-held lock. Refs: MGIT-120
package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/store/lock"
)

// TestLockTimeout_ConfiguredValue_IsHonoredByTheCLI holds the repo lock from
// the test process and asserts a contended command gives up after the
// CONFIGURED wait — naming it — instead of the 30s default it would otherwise
// sit through. Refs: FR-4.7, MGIT-120
func TestLockTimeout_ConfiguredValue_IsHonoredByTheCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lock-contention test")
	}
	bin := buildMgitBinary(t)
	repo := t.TempDir()

	out, err := runMgitLong(t, bin, repo, "init")
	require.NoError(t, err, "init: %s", out)
	out, err = runMgitLong(t, bin, repo, "config", "set", "locks.timeout_seconds", "2")
	require.NoError(t, err, "config set: %s", out)

	// Hold the lock the way another mgit process would.
	held, err := lock.Acquire(repo+"/.mgit", lock.DefaultTimeout)
	require.NoError(t, err, "test could not take the repo lock")
	defer func() { _ = held.Release() }()

	start := time.Now()
	out, err = runMgitLong(t, bin, repo, "status")
	elapsed := time.Since(start)

	require.Error(t, err, "a contended command must fail, not proceed: %s", out)
	assert.Contains(t, out, "another mgit process is running")
	assert.Contains(t, out, "after 2s",
		"the refusal must report the CONFIGURED wait, not the built-in default: %s",
		strings.TrimSpace(out))
	assert.Less(t, elapsed, 20*time.Second,
		"a 2s configured wait must not sit through the 30s default")
}

// TestLockTimeout_Unconfigured_KeepsThirtySecondDefault proves the knob did not
// change out-of-the-box behavior: with no `locks` section, the refusal still
// names the historical 30s wait. Refs: MGIT-120
func TestLockTimeout_Unconfigured_KeepsThirtySecondDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lock-contention test")
	}
	bin := buildMgitBinary(t)
	repo := t.TempDir()

	out, err := runMgitLong(t, bin, repo, "init")
	require.NoError(t, err, "init: %s", out)

	held, err := lock.Acquire(repo+"/.mgit", lock.DefaultTimeout)
	require.NoError(t, err, "test could not take the repo lock")
	defer func() { _ = held.Release() }()

	out, err = runMgitLong(t, bin, repo, "status")
	require.Error(t, err, "a contended command must fail, not proceed: %s", out)
	assert.Contains(t, out, "after 30s", "the default wait must be unchanged: %s", strings.TrimSpace(out))
}
