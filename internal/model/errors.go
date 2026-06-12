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

	// ErrFileNotFound indicates a path is absent from a commit's tree.
	// Refs: FR-6.7
	ErrFileNotFound = errors.New("file not found in commit")

	// ErrSandboxNotFound indicates a sandbox ID or task resolves to no
	// registered sandbox. Refs: FR-17.20
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxBackendUnavailable indicates no hypervisor backend is
	// available on this platform (and the container fallback was not
	// explicitly acknowledged). Refs: FR-17.15, FR-17.20
	ErrSandboxBackendUnavailable = errors.New("no sandbox backend available on this platform")

	// ErrLandVerificationFailed indicates dual-hash or task-binding
	// verification failed during sandbox land; nothing was imported.
	// Refs: FR-17.5, FR-17.20, FR-17.24
	ErrLandVerificationFailed = errors.New("sandbox land: commit verification failed")

	// ErrUnlandedCommits indicates a sandbox still holds commits that
	// have not been landed; remove requires --force. Refs: FR-17.19, FR-17.20
	ErrUnlandedCommits = errors.New("sandbox has unlanded commits")

	// ErrNetworkPolicyViolation indicates a guest flow was denied by the
	// host-enforced network policy. Refs: FR-17.7, FR-17.8, FR-17.20
	ErrNetworkPolicyViolation = errors.New("network policy violation")

	// ErrUnattestedCommit indicates a commit lacks a valid host-issued
	// sandbox attestation while require_sandbox is enabled.
	// Refs: FR-17.6, FR-17.20
	ErrUnattestedCommit = errors.New("commit lacks sandbox attestation")

	// ErrSensitivePathModified indicates the guest modified a protected
	// host-trusted path (e.g. .claude/**, .git/hooks/**); land refuses.
	// Refs: FR-17.14, FR-17.20
	ErrSensitivePathModified = errors.New("guest modified a protected host-trusted path")
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
