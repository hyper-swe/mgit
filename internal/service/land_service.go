package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"golang.org/x/sync/singleflight"

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

// LandLimits bounds the land path's worst-case host memory. Each in-flight
// land buffers one guest pool in RAM, bounded per-pool by MaxPoolBytes (the
// per-pool ceiling the LandChannel enforces, land.DefaultLimits().MaxTotalBytes).
// MaxConcurrentLands caps how many pulls may be buffered AT ONCE, so the
// documented host budget is exactly:
//
//	worst-case buffered RAM = MaxConcurrentLands × MaxPoolBytes
//
// This aggregate bound is SEPARATE from the daemon's MaxConns (which bounds
// connections, not land memory) and from land.DecodeObjects' per-object/count/
// total ceilings (which bound ONE pool). A hostile guest must never be able to
// drive an unbounded number of concurrent 4 GiB buffers. Refs: FR-17.35, MGIT-11.13.5
type LandLimits struct {
	// MaxConcurrentLands is the maximum number of lands buffering a pool at
	// once. Beyond it a new land queues (bounded by its context deadline) and,
	// if it cannot acquire a slot in time, is rejected rather than over-
	// allocating. Must be > 0.
	MaxConcurrentLands int
	// MaxPoolBytes is the per-pool ceiling used to document the host budget. It
	// mirrors the LandChannel's per-pool limit; the channel is the enforcement
	// point, this field is for the budget calculation and audit logging.
	MaxPoolBytes int64
}

// Default land-concurrency bounds. The concurrency cap is intentionally small:
// lands are heavyweight (verify + import) and single-client in normal use, so a
// handful of concurrent lands is ample headroom while keeping the worst-case
// host budget modest (4 × 4 GiB = 16 GiB with the default per-pool ceiling).
const (
	defaultMaxConcurrentLands = 4
	defaultMaxPoolBytes       = 4 << 30 // mirrors land.DefaultLimits().MaxTotalBytes
)

// DefaultLandLimits returns the safe default land-concurrency bounds.
func DefaultLandLimits() LandLimits {
	return LandLimits{
		MaxConcurrentLands: defaultMaxConcurrentLands,
		MaxPoolBytes:       defaultMaxPoolBytes,
	}
}

// LandOption configures optional LandService behavior without breaking the
// required-dependency constructor signature.
type LandOption func(*LandService)

// WithLandLimits sets the concurrency cap and per-pool budget. Non-positive
// fields fall back to the safe defaults (and the applied values are logged).
func WithLandLimits(limits LandLimits) LandOption {
	return func(s *LandService) { s.limits = limits }
}

// WithLandLogger wires a logger for budget/rejection audit events. A nil logger
// is ignored (the discard default stays). Refs: MGIT-11.13.5
func WithLandLogger(logger *slog.Logger) LandOption {
	return func(s *LandService) {
		if logger != nil {
			s.logger = logger
		}
	}
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

	limits LandLimits
	logger *slog.Logger
	// sem bounds concurrent in-flight lands: a slot is taken before the pool is
	// pulled (buffered) and released when the land completes. At capacity a land
	// queues on its context and is rejected if it cannot acquire in time, so the
	// worst-case buffered RAM is MaxConcurrentLands × MaxPoolBytes — never
	// unbounded. SEPARATE from the daemon's MaxConns. Refs: MGIT-11.13.5
	sem chan struct{}
	// flight coalesces concurrent lands for the SAME sandbox into one in-flight
	// pull (F7, MGIT-11.10.11): a hostile guest spamming its notify socket fans
	// out many triggers for one sandbox; without this they race the single
	// pools[sandboxID] buffer slot (last-writer-wins waste). Keyed by sandbox ID
	// so it covers BOTH the notify trigger and the control-plane verb, and it
	// composes with sem (the leader holds one sem slot; coalesced followers hold
	// none). Refs: MGIT-11.10.11, MGIT-11.13.5
	flight singleflight.Group
}

// NewLandService wires the land service. All dependencies are required.
// Timestamping is owned by the orchestrator and attestor (clock-injected
// there), so the service holds no clock of its own.
func NewLandService(resolver SandboxResolver, puller LandPuller, ledger LandedCommitReader,
	parents poolParentResolver, attestor Attestor, orch landOrchestrator,
	policy SandboxPolicyReader, opts ...LandOption) (*LandService, error) {
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
	s := &LandService{
		resolver: resolver, puller: puller, ledger: ledger, parents: parents,
		attestor: attestor, orch: orch, policy: policy,
		limits: DefaultLandLimits(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(s)
	}
	// Apply safe defaults for any unset/invalid bound, so a misconfigured cap
	// can never disable the memory budget (a zero-length semaphore would also
	// deadlock every land). Refs: MGIT-11.13.5
	if s.limits.MaxConcurrentLands <= 0 {
		s.limits.MaxConcurrentLands = defaultMaxConcurrentLands
	}
	if s.limits.MaxPoolBytes <= 0 {
		s.limits.MaxPoolBytes = defaultMaxPoolBytes
	}
	s.sem = make(chan struct{}, s.limits.MaxConcurrentLands)
	// Log the effective bounds and the derived host budget so the operator can
	// audit that cap × per-pool fits the host (no silent budget). Refs: MGIT-11.13.5
	s.logger.Info("land concurrency bounds applied",
		"event", "land_budget",
		"max_concurrent_lands", s.limits.MaxConcurrentLands,
		"max_pool_bytes", s.limits.MaxPoolBytes,
		"host_memory_budget_bytes", int64(s.limits.MaxConcurrentLands)*s.limits.MaxPoolBytes)
	return s, nil
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

	// Per-sandbox single-flight (F7): coalesce concurrent triggers for ONE
	// sandbox into a single in-flight land. A hostile guest spamming its notify
	// socket fans out many Land calls for its own sandbox; without coalescing
	// they each pull a full pool and race the single pools[sandboxID] buffer
	// (self-inflicted resource waste, last-writer-wins). With it, the first call
	// runs the land and any concurrent caller for the same sandbox shares its
	// result instead of launching a second pull. Keyed by sandbox ID so it also
	// covers the control-plane verb. Refs: MGIT-11.10.11, MGIT-11.13.5
	v, err, _ := s.flight.Do(sandboxID, func() (interface{}, error) {
		return s.landGuarded(ctx, tid, sandboxID)
	})
	if err != nil {
		return nil, err
	}
	return v.(*LandSummary), nil
}

// landGuarded acquires a concurrency slot (bounding worst-case buffered RAM to
// MaxConcurrentLands × MaxPoolBytes), runs one land, and releases the slot. At
// capacity it queues on the context and is REJECTED (never over-allocating) if
// a slot does not free in time. The single-flight leader runs this; coalesced
// followers do not, so concurrent triggers for one sandbox consume one slot.
// Refs: MGIT-11.13.5
func (s *LandService) landGuarded(ctx context.Context, tid model.TaskID, sandboxID string) (*LandSummary, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.logger.Warn("land rejected: concurrency cap reached",
			"event", "land_rejected", "sandbox_id", sandboxID,
			"max_concurrent_lands", s.limits.MaxConcurrentLands, "error", ctx.Err().Error())
		return nil, fmt.Errorf("sandbox land: concurrency cap of %d reached: %w",
			s.limits.MaxConcurrentLands, ctx.Err())
	}
	return s.landOnce(ctx, tid, sandboxID)
}

// landOnce performs one land for an already-resolved sandbox: pull the pool
// once, derive the new chain, and route through the orchestrator. It is invoked
// under both the per-sandbox single-flight and the global concurrency slot.
func (s *LandService) landOnce(ctx context.Context, tid model.TaskID, sandboxID string) (*LandSummary, error) {
	taskID := tid.String()
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
