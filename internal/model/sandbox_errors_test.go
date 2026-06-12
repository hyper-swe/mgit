// Package model defines pure domain types for mgit.
// These tests verify the FR-17 sandbox sentinel errors per MGIT-11.2.1
// acceptance criteria. Refs: FR-17.20, NFR-3
package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// sandboxSentinels lists every FR-17.20 sandbox sentinel error.
func sandboxSentinels() []error {
	return []error{
		ErrSandboxNotFound,
		ErrSandboxBackendUnavailable,
		ErrLandVerificationFailed,
		ErrUnlandedCommits,
		ErrNetworkPolicyViolation,
		ErrUnattestedCommit,
		ErrSensitivePathModified,
	}
}

// TestErrors_SandboxSentinelsDefined verifies all seven FR-17.20
// sentinels are defined with non-empty messages. Refs: FR-17.20
func TestErrors_SandboxSentinelsDefined(t *testing.T) {
	sentinels := sandboxSentinels()
	assert.Len(t, sentinels, 7, "FR-17.20 defines exactly seven sandbox sentinels")

	for _, err := range sentinels {
		t.Run(err.Error(), func(t *testing.T) {
			assert.NotEmpty(t, err.Error(), "error message must not be empty")
		})
	}
}

// TestErrors_DistinctMessages verifies no two sandbox sentinels (or any
// existing model sentinel) share a message, so errors.Is targets stay
// unambiguous in logs and audit records. Refs: FR-17.20
func TestErrors_DistinctMessages(t *testing.T) {
	all := append(sandboxSentinels(),
		ErrCommitNotFound, ErrTaskNotFound, ErrBranchNotFound,
		ErrAppendOnlyViolation, ErrVerificationFailed,
		ErrTaskAlreadyBound, ErrTaskMismatch, ErrWorktreeNotFound,
	)

	seen := make(map[string]bool, len(all))
	for _, err := range all {
		assert.False(t, seen[err.Error()],
			"duplicate sentinel message: %q", err.Error())
		seen[err.Error()] = true
	}
}

// TestErrors_WrappableWithIs verifies every sandbox sentinel survives
// %w wrapping and is matched by errors.Is, and that distinct sentinels
// never match each other. Refs: FR-17.20
func TestErrors_WrappableWithIs(t *testing.T) {
	sentinels := sandboxSentinels()

	for _, sentinel := range sentinels {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("sandbox srv-01: %w", sentinel)
			assert.True(t, errors.Is(wrapped, sentinel),
				"wrapped sentinel must match with errors.Is")

			doubly := fmt.Errorf("land MGIT-4.2: %w", wrapped)
			assert.True(t, errors.Is(doubly, sentinel),
				"doubly wrapped sentinel must match with errors.Is")

			for _, other := range sentinels {
				if errors.Is(sentinel, other) {
					continue // the sentinel itself
				}
				assert.False(t, errors.Is(wrapped, other),
					"%q must not match %q", sentinel, other)
			}
		})
	}
}
