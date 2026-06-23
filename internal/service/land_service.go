package service

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// SandboxResolver resolves the sandbox bound to a task, host-anchored: the
// returned ID is the launch-assigned sandbox identity, never guest-asserted.
// *SandboxService satisfies it. Refs: FR-17.5, SEC-05
type SandboxResolver interface {
	Status(ctx context.Context, taskID string) (*model.SandboxInfo, error)
}

// LandPuller pulls a sandbox's land object pool over the peer-authorized
// channel once and buffers it for the orchestrator to replay (SEC-06,
// SEC-10), returning the decoded pool for host-side batch derivation. Discard
// drops the buffer when a land is abandoned before the orchestrator runs.
// *sandboxd.LandChannel satisfies it. Refs: FR-17.5, SEC-06, SEC-10
type LandPuller interface {
	Pull(ctx context.Context, sandboxID string) ([]land.Object, error)
	Discard(sandboxID string)
}

// LandedCommitReader reports a task's already-landed commits — a READ-ONLY
// view used for the base position and to exclude base history from the new
// batch. It deliberately excludes the append path so the land service cannot
// write task_commits except through the orchestrator's persister.
// *index.Store satisfies it. Refs: FR-17.5
type LandedCommitReader interface {
	GetTaskCommits(ctx context.Context, taskID string) ([]index.CommitRecord, error)
}

// Attestor issues a host attestation for an observed commit (SEC-01): it
// signs only the (sandboxID, commitHash, contentHash) the host itself
// computed from the bytes it read. *attest.Service satisfies it. Refs: FR-17.6, SEC-01
type Attestor interface {
	Attest(ctx context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error)
}

// poolParentResolver is the pool-aware parent resolver the land service both
// registers the pulled pool with (so intra-batch parents resolve) and reads
// during derivation. *land.PoolAwareParentResolver satisfies it; the SAME
// instance is the orchestrator's ParentTreeResolver, so derivation and
// re-verification resolve parents identically. Refs: SEC-06, FR-17.5
type poolParentResolver interface {
	ParentFileSet(ctx context.Context, parentCommitID string) (map[string]string, error)
	// HostHasCommit reports whether the host shared store already contains the
	// commit, so base history the guest re-streamed is excluded from the new
	// batch even when it is not in the task_commits ledger (e.g. the branch base
	// or genesis is host history, never a task commit). Host-only. Refs: FR-17.5
	HostHasCommit(ctx context.Context, commitID string) (bool, error)
	Register(pool []land.Object) ([]string, error)
	Deregister(ids []string)
}

// landOrchestrator is the verify+persist chokepoint the land service routes
// through exclusively. *LandOrchestrator satisfies it. Refs: FR-17.5
type landOrchestrator interface {
	Land(ctx context.Context, req LandRequest) error
}

// LandSummary reports the outcome of a land for the CLI.
type LandSummary struct {
	Commits int    // number of new commits landed (0 = nothing new)
	Branch  string // the task branch advanced
}

// LandService coordinates a land over the control plane: resolve the
// host-bound sandbox, pull its object pool once (peer-authorized, single read
// for SEC-06), derive the verified commit batch and host attestations from
// the pulled bytes, and route EXCLUSIVELY through the LandOrchestrator — the
// single verify+persist chokepoint. It holds NO persister/importer/brancher:
// the shared store is written only inside the orchestrator. DI everywhere;
// clock injected. Refs: FR-17.5, FR-17.6, FR-17.24, SEC-01, SEC-06, SEC-10
type LandService struct {
	resolver SandboxResolver
	puller   LandPuller
	ledger   LandedCommitReader
	parents  poolParentResolver
	attestor Attestor
	orch     landOrchestrator
	policy   SandboxPolicyReader
}

// NewLandService wires the land service. All dependencies are required.
// Timestamping is owned by the orchestrator and attestor (clock-injected
// there), so the service holds no clock of its own.
func NewLandService(resolver SandboxResolver, puller LandPuller, ledger LandedCommitReader,
	parents poolParentResolver, attestor Attestor, orch landOrchestrator,
	policy SandboxPolicyReader) (*LandService, error) {
	switch {
	case resolver == nil:
		return nil, fmt.Errorf("land service: sandbox resolver must not be nil")
	case puller == nil:
		return nil, fmt.Errorf("land service: land puller must not be nil")
	case ledger == nil:
		return nil, fmt.Errorf("land service: commit ledger must not be nil")
	case parents == nil:
		return nil, fmt.Errorf("land service: parent resolver must not be nil")
	case attestor == nil:
		return nil, fmt.Errorf("land service: attestor must not be nil")
	case orch == nil:
		return nil, fmt.Errorf("land service: orchestrator must not be nil")
	case policy == nil:
		return nil, fmt.Errorf("land service: policy reader must not be nil")
	}
	return &LandService{
		resolver: resolver, puller: puller, ledger: ledger, parents: parents,
		attestor: attestor, orch: orch, policy: policy,
	}, nil
}

// Land lands a task's new sandbox commits onto its host branch. It resolves
// the bound sandbox, pulls its pool once, derives the new commit chain (base
// history excluded) with host attestations, and routes the batch through the
// orchestrator, which re-verifies and persists atomically. A land with no
// new commits is a success no-op. Verification failure imports nothing.
// Refs: FR-17.5, FR-17.6, FR-17.24, SEC-01, SEC-06
func (s *LandService) Land(ctx context.Context, taskID string) (*LandSummary, error) {
	tid, err := model.ParseTaskID(taskID)
	if err != nil {
		return nil, fmt.Errorf("sandbox land: %w", err)
	}
	info, err := s.resolver.Status(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("sandbox land: resolve sandbox: %w", err)
	}
	sandboxID := info.ID

	pool, err := s.puller.Pull(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("sandbox land: pull pool: %w", err)
	}

	chain, basePos, err := s.newChain(ctx, taskID, pool)
	if err != nil {
		s.puller.Discard(sandboxID)
		return nil, err
	}
	if len(chain) == 0 {
		s.puller.Discard(sandboxID) // nothing new: drop the buffered pool
		return &LandSummary{Commits: 0, Branch: model.TaskBranchName(taskID)}, nil
	}

	req, err := s.buildRequest(ctx, tid, sandboxID, basePos, pool, chain)
	if err != nil {
		s.puller.Discard(sandboxID)
		return nil, err
	}
	if err := s.orch.Land(ctx, req); err != nil {
		return nil, err // the orchestrator consumed the buffer; nothing imported on failure
	}
	return &LandSummary{Commits: len(chain), Branch: model.TaskBranchName(taskID)}, nil
}

// newChain derives the ordered new commit chain (excluding base history
// already in task_commits) and the base position to append at.
func (s *LandService) newChain(ctx context.Context, taskID string, pool []land.Object) ([]land.PoolCommit, int, error) {
	existing, err := s.ledger.GetTaskCommits(ctx, taskID)
	if err != nil {
		return nil, 0, fmt.Errorf("sandbox land: read task commits: %w", err)
	}
	landed := make(map[string]bool, len(existing))
	for _, rec := range existing {
		landed[rec.CommitHash] = true
	}
	// A commit is base history (not new) if it is already in the task ledger OR
	// already in the host store — the guest re-streams everything reachable from
	// its branch head, including the branch base/genesis which is host history
	// but never a task commit. Excluding only the ledger would re-land the base.
	// Refs: FR-17.5, SEC-06, MGIT-11.9.6
	var skipErr error
	skip := func(id string) bool {
		if landed[id] {
			return true
		}
		has, err := s.parents.HostHasCommit(ctx, id)
		if err != nil {
			skipErr = err
		}
		return has
	}
	chain, err := land.CommitChain(pool, skip)
	if skipErr != nil {
		return nil, 0, fmt.Errorf("sandbox land: host commit check: %w", skipErr)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("sandbox land: %w", err)
	}
	return chain, len(existing), nil
}

// buildRequest derives a fully-bound, host-attested ClaimedCommit for each
// commit in the chain and assembles the land request. The pool is registered
// so intra-batch parents resolve; the orchestrator (same resolver) re-binds
// every commit before importing. Attestations are issued only under
// require_sandbox, from hashes the host computed (SEC-01). Refs: FR-17.5, FR-17.6, SEC-01
func (s *LandService) buildRequest(ctx context.Context, tid model.TaskID, sandboxID string,
	basePos int, pool []land.Object, chain []land.PoolCommit) (LandRequest, error) {
	requireSandbox, err := s.requireSandbox(ctx)
	if err != nil {
		return LandRequest{}, err
	}
	ids, err := s.parents.Register(pool)
	if err != nil {
		return LandRequest{}, fmt.Errorf("sandbox land: %w", err)
	}
	defer s.parents.Deregister(ids)

	commits := make([]ClaimedCommit, 0, len(chain))
	for _, pc := range chain {
		cc, err := s.claimCommit(ctx, tid, sandboxID, pool, pc, requireSandbox)
		if err != nil {
			return LandRequest{}, err
		}
		commits = append(commits, cc)
	}
	return LandRequest{TaskID: tid.String(), SandboxID: sandboxID, BasePosition: basePos, Commits: commits}, nil
}

// claimCommit derives one commit from its bytes and issues its host
// attestation when require_sandbox is on.
func (s *LandService) claimCommit(ctx context.Context, tid model.TaskID, sandboxID string,
	pool []land.Object, pc land.PoolCommit, requireSandbox bool) (ClaimedCommit, error) {
	parentFiles, err := s.parents.ParentFileSet(ctx, pc.ParentID)
	if err != nil {
		return ClaimedCommit{}, fmt.Errorf("sandbox land: resolve parent tree: %w", err)
	}
	c, err := land.DeriveLandedCommit(pc.Data, pool, tid, parentFiles)
	if err != nil {
		return ClaimedCommit{}, fmt.Errorf("sandbox land: %w", err)
	}
	var att *model.Attestation
	if requireSandbox {
		// Sign only the (sandboxID, hashes) the host computed from the bytes,
		// never guest-asserted values (SEC-01).
		att, err = s.attestor.Attest(ctx, sandboxID, c.CommitID, c.ContentHash)
		if err != nil {
			return ClaimedCommit{}, fmt.Errorf("sandbox land: attest commit: %w", err)
		}
	}
	return ClaimedCommit{Commit: c, Attestation: att}, nil
}

// requireSandbox reads the effective require_sandbox policy at land time, so
// the client cannot influence it (the land request carries no policy field).
// Refs: FR-17.6, SEC-05
func (s *LandService) requireSandbox(ctx context.Context) (bool, error) {
	policy, err := s.policy.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("sandbox land: load policy: %w", err)
	}
	return policy.RequireSandbox, nil
}
