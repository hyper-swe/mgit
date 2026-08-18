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

	// ErrNothingToCommit indicates a commit was requested that would record no
	// change: the tree it would produce is byte-identical to its parent's. mgit
	// refuses it unless emptiness is requested explicitly, because a hash
	// returned for unrecorded work is a false success signal to an agent.
	// Refs: FR-2, MGIT-77
	ErrNothingToCommit = errors.New("nothing to commit")

	// ErrFileTooLarge indicates a file offered for staging exceeds the
	// staged-file size limit. mgit's store is append-only, so a large blob
	// staged by accident — nearly always a locally built binary swept in by a
	// bulk stage — bloats the task branch permanently, even after a later
	// commit deletes it again. The refusal is deliberate and overridable.
	// Refs: FR-2.6b, MGIT-131, MGIT-80
	ErrFileTooLarge = errors.New("file exceeds the staged-file size limit")

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

	// ErrContentConflict indicates a diff cannot be applied because the target
	// tree's content at a path no longer matches the diff's expected old state
	// (the file changed since the diff was computed). Content-applying verbs
	// (rollback, cherry-pick) fail with this rather than silently clobbering.
	// Refs: MGIT-54
	ErrContentConflict = errors.New("content conflict")

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

	// ErrWorktreeExists indicates a worktree is already registered at a path.
	// It is the third of the three FR-16 exclusivity refusals (alongside
	// ErrTaskAlreadyBound and ErrBranchInUse) and is what the loser of a race
	// to provision the same path sees. Refs: FR-16, MGIT-120
	ErrWorktreeExists = errors.New("worktree already registered at path")

	// ErrFileNotFound indicates a path is absent from a commit's tree.
	// Refs: FR-6.7
	ErrFileNotFound = errors.New("file not found in commit")

	// ErrAmbiguousHash indicates an abbreviated commit hash prefix matched
	// more than one commit and so could not be resolved unambiguously.
	// Refs: FR-3, FR-8.7, MGIT-18
	ErrAmbiguousHash = errors.New("ambiguous commit hash prefix")

	// ErrSandboxNotFound indicates a sandbox ID or task resolves to no
	// registered sandbox. Refs: FR-17.20
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxBooted indicates an operation that only makes sense for a
	// sandbox whose VM has NOT booted was asked of one that has. It is the
	// answer to a lost race: the egress-policy path decides, from the recorded
	// state, whether to stage a policy onto a pending launch or to mutate a
	// live enforcer, and a boot that lands between those two steps must never
	// end with a staged policy reported as enforced. The caller re-routes to
	// the live enforcer rather than reporting success. Refs: MGIT-109, MGIT-72, SEC-04
	ErrSandboxBooted = errors.New("sandbox has already booted")

	// ErrSandboxBackendUnavailable indicates no hypervisor backend is
	// available on this platform (and the container fallback was not
	// explicitly acknowledged). Refs: FR-17.15, FR-17.20
	ErrSandboxBackendUnavailable = errors.New("no sandbox backend available on this platform")

	// ErrSandboxSyncUnsupported indicates this sandbox's backend cannot
	// propagate host worktree changes into a RUNNING guest. Firecracker
	// delivers the worktree as an ext4 image built at launch and mounted by
	// the guest, so the host cannot write into it without corrupting it; such
	// a sandbox keeps launch-time-copy semantics and must be re-launched to
	// pick up host changes. Reported rather than papered over: a sync that
	// silently no-ops and claims success is how stale code gets executed.
	// Refs: MGIT-71, MGIT-76, ADR-011
	ErrSandboxSyncUnsupported = errors.New("this sandbox backend cannot sync a running guest's worktree")

	// ErrWorktreeSyncConflict indicates a host->guest worktree sync was
	// refused because the guest changed paths the host also changed. Nothing
	// is applied — not even the unblocked paths — and the conflicting paths
	// are named. Land the guest's work, or force the sync and accept that
	// each overwritten path is destroyed and recorded. Refs: MGIT-71, ADR-011
	ErrWorktreeSyncConflict = errors.New("worktree sync blocked by guest-side changes")

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

	// ErrAttestationInvalid indicates an attestation is present but does
	// not verify: a bad/forged signature, an unknown key_id, an
	// unsupported algorithm, or a tampered field. Distinct from
	// ErrUnattestedCommit (no attestation at all). Refs: FR-17.6, SEC-01
	ErrAttestationInvalid = errors.New("sandbox attestation does not verify")

	// ErrSensitivePathModified indicates the guest modified a protected
	// host-trusted path (e.g. .claude/**, .git/hooks/**); land refuses.
	// Refs: FR-17.14, FR-17.20
	ErrSensitivePathModified = errors.New("guest modified a protected host-trusted path")

	// ErrSharedStoreReachable indicates a guest filesystem plan would make
	// the host shared object store (.mgit objects/refs/index) resolvable
	// from inside the guest — a quarantine breach that would let the guest
	// read other tasks' objects or write the shared store directly,
	// bypassing the verified land door. The plan is rejected. Refs: SEC-03
	ErrSharedStoreReachable = errors.New("shared object store reachable from the guest")

	// ErrGuestNotServing reports a sandbox whose VMM started but whose guest
	// never answered on its control channel, so the launch failed CLOSED and
	// the sandbox was torn down rather than reported as running. Refs: MGIT-92
	ErrGuestNotServing = errors.New("guest never answered on its control channel")

	// ErrSandboxDaemonUnresponsive reports that mgit-sandboxd stopped emitting
	// liveness beats on an exec stream that was still open: the DAEMON
	// stalled. Neither the command nor the guest is implicated — a beating
	// daemon keeps beating however long a build takes, so silence is a
	// statement about the daemon and nothing else.
	//
	// It is a distinct sentinel because the diagnosis hangs off it. Every
	// other way an exec fails mid-flight points at the guest, so a daemon
	// stall reported through that path arrives dressed as a guest lost
	// mid-command with a memory-cap advisory attached — the misdiagnosis
	// class MGIT-118 is named for. Refs: MGIT-133, MGIT-118
	ErrSandboxDaemonUnresponsive = errors.New("sandbox daemon stopped answering (no liveness beat)")

	// ErrSandboxCeilingExceeded indicates a launch would exceed the
	// host-wide concurrency or memory ceiling; the launch fails fast
	// rather than degrading the host (SEC-09). Refs: FR-17.26
	ErrSandboxCeilingExceeded = errors.New("global sandbox ceiling exceeded")

	// ErrSandboxResourceLimitExceeded indicates ONE launch declared more
	// CPU, memory, or disk than the host policy's per-sandbox maximum
	// allows. Deliberately distinct from ErrSandboxCeilingExceeded: "this
	// launch is too big" and "the fleet is full" have different fixes —
	// lower the request (or have the operator raise the maximum) versus
	// free a running sandbox. The request is refused, never clamped.
	// Refs: R-H212, NFR-17.5, FR-17.26
	ErrSandboxResourceLimitExceeded = errors.New("per-sandbox resource limit exceeded")

	// ErrPeerBindingMismatch indicates a vsock/HvSocket connection's
	// hypervisor peer identity (CID or VM-GUID) does not match the
	// sandbox channel it addresses — one guest may never reach another's
	// land/attestation channel. Refs: FR-17.27, SEC-10
	ErrPeerBindingMismatch = errors.New("sandbox channel peer identity mismatch")

	// ErrConfineAgentDisabled indicates a T2 fully-confined-agent session
	// (credential injection / shell attach) was requested for a sandbox
	// whose policy has confine_agent off. T2 is strictly opt-in; the
	// default topology is T1 (agent on the host, commands routed in).
	// Refs: FR-17, ADR-005, MGIT-11.11.4
	ErrConfineAgentDisabled = errors.New("confine_agent is disabled (T2 is opt-in; default is T1)")

	// ErrShellTransportUnavailable indicates an interactive `mgit sandbox
	// shell` attach was requested but the bidirectional vsock-PTY guest
	// transport is not served by this daemon build (it is KVM-gated guest
	// infrastructure). The host-side T2 orchestration is in place; this is
	// the remaining guest-backend gap, reported rather than degraded to a
	// non-interactive session. Refs: MGIT-11.11.4
	ErrShellTransportUnavailable = errors.New("interactive shell requires the KVM guest PTY transport; use `mgit sandbox exec` for non-interactive commands")

	// ErrCapabilityGrantNotFound indicates no live capability grant exists
	// for the requested (sandbox, capability) pair — a grant dies with its
	// sandbox (scoped to the sandbox lifetime), so a lookup after teardown
	// is expected to miss. Refs: FR-17.12, SEC-05
	ErrCapabilityGrantNotFound = errors.New("capability grant not found")

	// ErrSSHKeyExtraction indicates the guest asked the host ssh-agent
	// forwarder for key MATERIAL (a private/identity blob), not a signature.
	// The host holds the keys and exposes signing only; key extraction is
	// refused unconditionally so private keys never enter the guest.
	// Refs: FR-17.12, SEC-01
	ErrSSHKeyExtraction = errors.New("ssh-agent forward exposes signing only; key material never crosses to the guest")

	// ErrArtifactExportUnsupported indicates this sandbox backend cannot
	// export a guest-built artifact to the host. It is a real limitation
	// reported rather than papered over: firecracker delivers the worktree as
	// a launch-time ext4 image the guest has mounted, so there is no host
	// directory to read the artifact out of, and the guest-mediated stream
	// that would be needed is not shipped in v1. Refs: MGIT-73, ADR-011
	ErrArtifactExportUnsupported = errors.New("this sandbox backend cannot export guest artifacts to the host")
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
