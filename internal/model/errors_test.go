// Package model defines pure domain types for mgit.
// These tests verify sentinel errors per MGIT-2.1.5 acceptance criteria.
// Refs: FR-12, NFR-3
package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrors_SentinelErrors(t *testing.T) {
	// Verify at least 15 sentinel errors are defined
	sentinels := []error{
		ErrCommitNotFound,
		ErrBranchNotFound,
		ErrTaskNotFound,
		ErrInvalidTaskID,
		ErrInvalidCommit,
		ErrBranchAlreadyExists,
		ErrBranchLocked,
		ErrMergeConflict,
		ErrIndexCorrupted,
		ErrSquashFailed,
		ErrRollbackFailed,
		ErrInvalidDiff,
		ErrSignatureInvalid,
		ErrAppendOnlyViolation,
		ErrStorageError,
		ErrChainBroken,
		ErrVerificationFailed,
		ErrBranchInUse,
		ErrTaskAlreadyBound,
		ErrTaskMismatch,
		ErrWorktreeNotFound,
		ErrRollbackConflict,
	}

	assert.GreaterOrEqual(t, len(sentinels), 15,
		"must define at least 15 sentinel errors")

	for _, err := range sentinels {
		t.Run(err.Error(), func(t *testing.T) {
			assert.NotNil(t, err)
			assert.NotEmpty(t, err.Error(), "error message must not be empty")
		})
	}
}

func TestErrors_ErrorIs(t *testing.T) {
	// errors.Is must work with sentinel errors
	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"direct_match", ErrCommitNotFound, ErrCommitNotFound, true},
		{"wrapped_match", fmt.Errorf("lookup: %w", ErrCommitNotFound), ErrCommitNotFound, true},
		{"double_wrapped", fmt.Errorf("svc: %w", fmt.Errorf("store: %w", ErrTaskNotFound)), ErrTaskNotFound, true},
		{"no_match", ErrCommitNotFound, ErrBranchNotFound, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errors.Is(tt.err, tt.target)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestErrors_ErrorAs(t *testing.T) {
	// Custom error types must work with errors.As
	original := &ValidationError{
		Field:   "task_id",
		Message: "must not be empty",
	}
	wrapped := fmt.Errorf("commit validation: %w", original)

	var target *ValidationError
	require.True(t, errors.As(wrapped, &target),
		"errors.As must extract ValidationError from wrapped error")
	assert.Equal(t, "task_id", target.Field)
	assert.Equal(t, "must not be empty", target.Message)
}

func TestErrors_ErrorWrapping(t *testing.T) {
	// Verify error wrapping preserves context
	inner := ErrCommitNotFound
	wrapped := fmt.Errorf("service layer: %w", inner)
	doubleWrapped := fmt.Errorf("handler: %w", wrapped)

	assert.True(t, errors.Is(doubleWrapped, ErrCommitNotFound))
	assert.Contains(t, doubleWrapped.Error(), "handler")
	assert.Contains(t, doubleWrapped.Error(), "service layer")
	assert.Contains(t, doubleWrapped.Error(), "commit not found")
}

func TestErrors_CustomErrorTypes(t *testing.T) {
	t.Run("ValidationError", func(t *testing.T) {
		err := &ValidationError{
			Field:   "commit_id",
			Message: "must be valid SHA-256",
		}
		assert.Contains(t, err.Error(), "commit_id")
		assert.Contains(t, err.Error(), "must be valid SHA-256")
	})

	t.Run("ConflictError", func(t *testing.T) {
		err := &ConflictError{
			Resource: "branch",
			ID:       "task/MGIT-1.2.3",
			Message:  "already locked by agent-02",
		}
		assert.Contains(t, err.Error(), "branch")
		assert.Contains(t, err.Error(), "task/MGIT-1.2.3")
		assert.Contains(t, err.Error(), "already locked by agent-02")
	})
}

func TestErrors_UniqueMessages(t *testing.T) {
	// All sentinel errors must have unique messages
	sentinels := []error{
		ErrCommitNotFound, ErrBranchNotFound, ErrTaskNotFound,
		ErrInvalidTaskID, ErrInvalidCommit, ErrBranchAlreadyExists,
		ErrBranchLocked, ErrMergeConflict, ErrIndexCorrupted,
		ErrSquashFailed, ErrRollbackFailed, ErrInvalidDiff,
		ErrSignatureInvalid, ErrAppendOnlyViolation, ErrStorageError,
	}

	seen := make(map[string]bool)
	for _, err := range sentinels {
		msg := err.Error()
		assert.False(t, seen[msg],
			"duplicate error message: %s", msg)
		seen[msg] = true
	}
}
