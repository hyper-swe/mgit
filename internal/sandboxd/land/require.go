package land

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// VerifyFunc verifies a host-issued attestation (model.Attestor.Verify).
// It is injected so the require_sandbox gate composes with the real host
// attestor without importing it.
type VerifyFunc func(ctx context.Context, att *model.Attestation) error

// EnforceRequireSandbox applies the require_sandbox policy to one commit
// at land time and returns the sandbox provenance to record in
// task_commits — a *string where nil means SQL NULL (the unsandboxed,
// permanently visible audit gap, F-02/SEC-02). Decisions:
//
//   - policy off: lands with NULL provenance; no attestation required
//     (the disablement itself is the audited gap, FR-17.6).
//   - policy on, no attestation: refused with ErrUnattestedCommit.
//   - policy on, attestation present but invalid: refused with the
//     verification error (ErrAttestationInvalid) — a forged attestation
//     never lands.
//   - policy on, attestation valid but issued for a DIFFERENT commit:
//     refused with ErrAttestationInvalid — an authentic attestation
//     captured from one commit must not land another (anti-replay,
//     SEC-01). The signature binds commit_hash + content_hash, so the
//     gate re-checks them against the commit actually being landed.
//   - policy on, valid attestation for this commit: lands with its sandbox_id.
//
// verify is consulted only under the policy, so a nil verifier is fine
// when require_sandbox is off. Refs: FR-17.6, F-02, SEC-01, SEC-02
func EnforceRequireSandbox(ctx context.Context, requireSandbox bool, commit *model.Commit, att *model.Attestation, verify VerifyFunc) (*string, error) {
	if !requireSandbox {
		return nil, nil //nolint:nilnil // (NULL sandbox_id, no error) is the policy-off outcome
	}
	if commit == nil {
		return nil, fmt.Errorf("%w: nil commit", model.ErrLandVerificationFailed)
	}
	if att == nil {
		return nil, fmt.Errorf("%w", model.ErrUnattestedCommit)
	}
	if err := verify(ctx, att); err != nil {
		return nil, err
	}
	// Anti-replay (SEC-01): the attestation must be FOR this commit. The
	// signature is authentic only for the (commit_hash, content_hash) it
	// was issued over; a captured attestation for another commit must not
	// confer sandbox provenance on this one.
	if att.CommitHash != commit.CommitID || att.ContentHash != commit.ContentHash {
		return nil, fmt.Errorf("%w: attestation is for commit %s, not %s",
			model.ErrAttestationInvalid, att.CommitHash, commit.CommitID)
	}
	sandboxID := att.SandboxID
	return &sandboxID, nil
}
