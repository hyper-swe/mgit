// Package model defines pure domain types for mgit.
// This file contains sentinel errors and custom error types
// used throughout the mgit codebase for consistent error handling.
// All sentinel errors are compatible with errors.Is() and errors.As().
// Refs: FR-12, NFR-3, MGIT-2.1.5
package model

import (
	"errors"
	"fmt"
)

// Sentinel errors for mgit operations.
// These are used with errors.Is() for type-safe error checking.
// Refs: FR-12 (audit), NFR-3 (reliability)
var (
	// ErrCommitNotFound indicates a commit does not exist in the store.
	ErrCommitNotFound = errors.New("commit not found")

	// ErrBranchNotFound indicates a branch does not exist.
	ErrBranchNotFound = errors.New("branch not found")

	// ErrTaskNotFound indicates a task ID has no associated commits.
	ErrTaskNotFound = errors.New("task not found")

	// ErrInvalidTaskID indicates a task ID does not match the expected format.
	ErrInvalidTaskID = errors.New("invalid task ID")

	// ErrInvalidCommit indicates a commit fails validation.
	ErrInvalidCommit = errors.New("invalid commit")

	// ErrBranchAlreadyExists indicates a branch name is already in use.
	ErrBranchAlreadyExists = errors.New("branch already exists")

	// ErrBranchLocked indicates a branch is locked by another agent.
	ErrBranchLocked = errors.New("branch locked")

	// ErrBranchInUse indicates a branch is checked out in another worktree.
	ErrBranchInUse = errors.New("branch checked out in another worktree")

	// ErrMergeConflict indicates a merge cannot be completed automatically.
	ErrMergeConflict = errors.New("merge conflict")

	// ErrIndexCorrupted indicates the SQLite index is in an inconsistent state.
	ErrIndexCorrupted = errors.New("index corrupted")

	// ErrSquashFailed indicates a squash operation could not be completed atomically.
	ErrSquashFailed = errors.New("squash failed")

	// ErrRollbackFailed indicates a rollback operation could not be completed.
	ErrRollbackFailed = errors.New("rollback failed")

	// ErrRollbackConflict indicates a rollback conflicts with current state.
	ErrRollbackConflict = errors.New("rollback conflict")

	// ErrInvalidDiff indicates a diff structure is malformed.
	ErrInvalidDiff = errors.New("invalid diff")

	// ErrSignatureInvalid indicates a commit signature failed verification.
	ErrSignatureInvalid = errors.New("signature invalid")

	// ErrAppendOnlyViolation indicates an attempt to modify or delete
	// immutable audit data. This is a critical safety violation.
	ErrAppendOnlyViolation = errors.New("append-only constraint violated")

	// ErrStorageError indicates a low-level storage operation failed.
	ErrStorageError = errors.New("storage error")

	// ErrChainBroken indicates the commit parent-child chain is inconsistent.
	ErrChainBroken = errors.New("commit chain broken")

	// ErrVerificationFailed indicates an integrity check did not pass.
	ErrVerificationFailed = errors.New("verification failed")

	// ErrTaskAlreadyBound indicates a task is already bound to a worktree.
	ErrTaskAlreadyBound = errors.New("task already bound to a worktree")

	// ErrTaskMismatch indicates a commit's task ID does not match the worktree binding.
	ErrTaskMismatch = errors.New("commit task ID does not match worktree binding")

	// ErrWorktreeNotFound indicates a worktree does not exist.
	ErrWorktreeNotFound = errors.New("worktree not found")
)

// ValidationError provides structured context for validation failures.
// It identifies which field failed and why, enabling precise error reporting.
// Refs: FR-3 (commit validation), FR-5 (branch validation)
type ValidationError struct {
	Field   string
	Message string
}

// Error implements the error interface for ValidationError.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s: %s", e.Field, e.Message)
}

// ConflictError provides context for resource conflicts.
// It identifies which resource is in conflict and the nature of the conflict.
// Refs: FR-5 (branch locking), FR-16 (worktree conflicts)
type ConflictError struct {
	Resource string
	ID       string
	Message  string
}

// Error implements the error interface for ConflictError.
func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict on %s %q: %s", e.Resource, e.ID, e.Message)
}
