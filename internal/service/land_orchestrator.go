package service

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// LandStreamOpener opens the guest's framed land object stream for a
// sandbox (the vsock land channel). The orchestrator depends on the byte
// stream, not on vsock; the concrete transport is wired in MGIT-11.9.2.
// Refs: FR-17.5, FR-17.35
type LandStreamOpener interface {
	OpenLandStream(ctx context.Context, sandboxID string) (io.ReadCloser, error)
}

// AttestationVerifier verifies a host-issued attestation's signature
// (internal/sandboxd/attest.Service.Verify). Refs: SEC-01, FR-17.6
type AttestationVerifier interface {
	Verify(ctx context.Context, att *model.Attestation) error
}

// LandPersister persists a verified batch atomically — import the object
// pool, append task_commits, fast-forward the task branch (land.Lander).
// It is the ONLY writer of the shared host store on the land path.
// Refs: FR-17.5
type LandPersister interface {
	Land(ctx context.Context, taskID string, pool []land.Object, commits []land.LandedCommit) error
}

// ParentTreeResolver resolves a commit's full file set (path -> blob hash)
// from the host store, so the SEC-06 tree binding can recompute the landed
// diff against it. An empty parent commit id yields the empty set (an
// initial commit). Host-side; the concrete host-git resolver is wired with
// the daemon land path (MGIT-11.10.10). Refs: SEC-06, FR-17.24
type ParentTreeResolver interface {
	ParentFileSet(ctx context.Context, parentCommitID string) (map[string]string, error)
}

// ClaimedCommit is one commit a guest asserts it produced, with the
// host-issued attestation it holds for it (nil only acceptable when
// require_sandbox is off). The metadata is UNTRUSTED until the orchestrator
// binds it to the object bytes pulled over the land channel. Refs: FR-17.5
type ClaimedCommit struct {
	Commit      *model.Commit
	Attestation *model.Attestation
}

// LandRequest is a guest's request to land a batch onto its bound task
// branch. Commits are in branch-append order; BasePosition is the position
// of the first commit on the task branch. Refs: FR-17.5
type LandRequest struct {
	TaskID       string
	SandboxID    string
	Commits      []ClaimedCommit
	BasePosition int
}

// LandOrchestrator runs the quarantine-then-land flow: it pulls a guest's
// objects over the land channel, schema-validates and binds every claimed
// commit to its bytes host-side, applies the require_sandbox gate, then
// persists the batch atomically and audits it. Handlers go through it,
// never the persister/stores directly. DI everywhere, clock injected.
// Refs: FR-17.5, FR-17.6, FR-17.24, SEC-01, SEC-02, SEC-06
type LandOrchestrator struct {
	streams     LandStreamOpener
	verifier    AttestationVerifier
	persist     LandPersister
	parentTrees ParentTreeResolver
	events      SandboxEventAppender
	policy      SandboxPolicyReader
	limits      land.Limits
	clock       func() time.Time
}

// NewLandOrchestrator wires the orchestrator. All dependencies are
// required (DI; no globals).
func NewLandOrchestrator(streams LandStreamOpener, verifier AttestationVerifier, persist LandPersister,
	parentTrees ParentTreeResolver, events SandboxEventAppender, policy SandboxPolicyReader,
	limits land.Limits, clock func() time.Time) (*LandOrchestrator, error) {
	switch {
	case streams == nil:
		return nil, fmt.Errorf("land orchestrator: stream opener must not be nil")
	case verifier == nil:
		return nil, fmt.Errorf("land orchestrator: attestation verifier must not be nil")
	case persist == nil:
		return nil, fmt.Errorf("land orchestrator: persister must not be nil")
	case parentTrees == nil:
		return nil, fmt.Errorf("land orchestrator: parent tree resolver must not be nil")
	case events == nil:
		return nil, fmt.Errorf("land orchestrator: event appender must not be nil")
	case policy == nil:
		return nil, fmt.Errorf("land orchestrator: policy reader must not be nil")
	case clock == nil:
		return nil, fmt.Errorf("land orchestrator: clock must not be nil")
	}
	return &LandOrchestrator{
		streams: streams, verifier: verifier, persist: persist, parentTrees: parentTrees,
		events: events, policy: policy, limits: limits, clock: clock,
	}, nil
}

// Land runs the full flow for one batch (FR-17.5): pull objects, decode +
// bound (schema-validate), bind every claimed commit to its bytes
// (dual-hash, SEC-06), apply require_sandbox (SEC-01/SEC-02), then persist
// atomically and audit. Every commit is verified before ANY is imported,
// so a single failure imports nothing. The shared store is written only by
// the persister, host-side. Refs: FR-17.5, FR-17.6, FR-17.24, SEC-06
func (o *LandOrchestrator) Land(ctx context.Context, req LandRequest) error {
	if err := validateLandRequest(req); err != nil {
		return err
	}
	policy, err := o.policy.Load(ctx)
	if err != nil {
		return fmt.Errorf("sandbox land: load policy: %w", err)
	}
	objsByID, pool, err := o.pullObjects(ctx, req.SandboxID)
	if err != nil {
		return err
	}
	landed, err := o.verifyBatch(ctx, req, policy.RequireSandbox, objsByID, pool)
	if err != nil {
		return err // nothing imported
	}
	// The decoded objects are one shared content-addressed pool for the
	// land, imported once by the persister (not partitioned per commit).
	if err := o.persist.Land(ctx, req.TaskID, pool, landed); err != nil {
		return fmt.Errorf("sandbox land: %w", err)
	}
	// Audit the landed lifecycle event AFTER the durable persist: the
	// authoritative per-commit provenance is the task_commits rows the
	// persister appended; this is the secondary sandbox-lifecycle marker,
	// so a failure here surfaces but the land is already durable.
	// Refs: FR-17.5, FR-17.18
	if err := o.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: req.SandboxID, TaskID: req.TaskID, EventType: model.EventLanded,
	}); err != nil {
		return fmt.Errorf("sandbox land: audit: %w", err)
	}
	return nil
}

// pullObjects opens the land channel, decodes the framed objects within the
// ceilings, and indexes the commit objects by id. The stream is always
// closed. Refs: FR-17.5, FR-17.35
func (o *LandOrchestrator) pullObjects(ctx context.Context, sandboxID string) (map[string][]byte, []land.Object, error) {
	stream, err := o.streams.OpenLandStream(ctx, sandboxID)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox land: open stream: %w", err)
	}
	defer stream.Close() //nolint:errcheck // read-only stream; close error is not actionable
	objs, err := o.limits.DecodeObjects(stream)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox land: %w", err)
	}
	byID, err := land.CommitObjectsByID(objs)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox land: %w", err)
	}
	return byID, objs, nil
}

// verifyBatch binds and gates every claimed commit, returning the landable
// batch only if ALL pass (so a failure persists nothing). Each commit is
// paired with its exact pulled bytes, bound to its task, hash-verified
// (SEC-06), then run through require_sandbox to derive its provenance
// (SEC-01/SEC-02). Refs: FR-17.5, FR-17.6, FR-17.24
func (o *LandOrchestrator) verifyBatch(ctx context.Context, req LandRequest, requireSandbox bool,
	objsByID map[string][]byte, pool []land.Object) ([]land.LandedCommit, error) {
	landed := make([]land.LandedCommit, 0, len(req.Commits))
	for i, cc := range req.Commits {
		objData, ok := objsByID[cc.Commit.CommitID]
		if !ok {
			return nil, fmt.Errorf("%w: no object for claimed commit %s",
				model.ErrLandVerificationFailed, cc.Commit.CommitID)
		}
		if cc.Commit.TaskID.String() != req.TaskID {
			return nil, fmt.Errorf("%w: commit %s is for task %s, not the bound task %s",
				model.ErrTaskMismatch, cc.Commit.CommitID, cc.Commit.TaskID.String(), req.TaskID)
		}
		if err := land.VerifyBinding(objData, cc.Commit); err != nil {
			return nil, err
		}
		// SEC-06 completeness: bind the CLAIMED FileDiffs to the actual
		// landed tree. The parent file set comes from the host store; the
		// recomputed diff must match the claim and every tree path must be
		// worktree-safe, or nothing lands. Refs: FR-17.24, FR-17.35, SEC-06
		parentFiles, err := o.parentTrees.ParentFileSet(ctx, cc.Commit.ParentID)
		if err != nil {
			return nil, fmt.Errorf("sandbox land: resolve parent tree: %w", err)
		}
		if err := land.VerifyTreeBinding(pool, cc.Commit, parentFiles); err != nil {
			return nil, err
		}
		sandboxID, err := land.EnforceRequireSandbox(ctx, requireSandbox, req.SandboxID, cc.Commit, cc.Attestation, o.verifier.Verify)
		if err != nil {
			return nil, err
		}
		landed = append(landed, land.LandedCommit{
			Commit: cc.Commit, SandboxID: sandboxID, Position: req.BasePosition + i,
		})
	}
	return landed, nil
}

// validateLandRequest rejects structurally invalid requests before any
// store or stream access. Refs: FR-17.5
func validateLandRequest(req LandRequest) error {
	switch {
	case req.TaskID == "":
		return fmt.Errorf("%w: empty task id", model.ErrLandVerificationFailed)
	case req.SandboxID == "":
		return fmt.Errorf("%w: empty sandbox id", model.ErrLandVerificationFailed)
	case len(req.Commits) == 0:
		return fmt.Errorf("%w: no commits to land", model.ErrLandVerificationFailed)
	case req.BasePosition < 0:
		return fmt.Errorf("%w: negative base position", model.ErrLandVerificationFailed)
	}
	for _, cc := range req.Commits {
		if cc.Commit == nil {
			return fmt.Errorf("%w: nil commit in batch", model.ErrLandVerificationFailed)
		}
	}
	return nil
}
