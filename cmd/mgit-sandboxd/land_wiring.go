package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/firecracker"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
	"github.com/hyper-swe/mgit/internal/service"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// buildLandService wires the control-plane land path and returns it behind
// the daemon's SandboxLander seam. It opens the host shared repository and
// the main task_commits index (derived from the host config root:
// <repo>/.mgit/sandbox -> <repo>), loads or first-generates the host
// attestation key, and assembles the verified LandOrchestrator + LandService.
// The daemon holds ONLY the returned lander; the persister and stores are
// reachable solely through the orchestrator (SEC-01 no-bypass). The single
// peer-authorized read closes the SEC-06 refetch window. A returned closer
// releases the repo + index this opens. Refs: MGIT-11.10.10, SEC-01, SEC-06, SEC-10
func buildLandService(hostRoot, repoRoot, workDir string, resolver service.SandboxResolver,
	events service.SandboxEventAppender, policy service.SandboxPolicyReader,
	peerBinder *sandboxd.PeerBinder, clock func() time.Time, logger *slog.Logger,
) (sandboxd.SandboxLander, func() error, error) {
	if repoRoot == "" {
		// Fallback for callers that do not pass the repo root explicitly:
		// recover it from the conventional <repo>/.mgit/sandbox host root.
		repoRoot = filepath.Dir(filepath.Dir(hostRoot))
	}
	repo, err := gitstore.Open(repoRoot, clock)
	if err != nil {
		return nil, nil, fmt.Errorf("open host repo %s: %w", repoRoot, err)
	}
	mainIdx, err := index.New(filepath.Join(repoRoot, ".mgit", "index.db"), clock)
	if err != nil {
		_ = repo.Close()
		return nil, nil, fmt.Errorf("open host index: %w", err)
	}
	closer := func() error { return errors.Join(mainIdx.Close(), repo.Close()) }

	attestor, err := loadOrGenerateAttestor(hostRoot, clock, logger)
	if err != nil {
		_ = closer()
		return nil, nil, err
	}

	// The Lander is the ONLY shared-store writer on the land path; it is
	// reachable only through the orchestrator below.
	lander := land.NewLander(
		land.NewStoreImporter(repo),
		mainIdx, // *index.Store satisfies land.CommitAppender (AppendTaskCommits)
		land.NewStoreBrancher(gitstore.NewMergeStore(repo)),
	)
	parents := land.NewPoolAwareParentResolver(land.NewHostParentTreeResolver(repo))
	// v1 land transport is the firecracker per-VM vsock socket (Linux KVM, the
	// proven land path); the dialer is pure host I/O over workDir.
	channel := sandboxd.NewLandChannel(peerBinder, firecracker.NewLandDialer(workDir), land.DefaultLimits(), logger)

	orch, err := service.NewLandOrchestrator(channel, attestor, lander, parents, events, policy, land.DefaultLimits(), clock)
	if err != nil {
		_ = closer()
		return nil, nil, fmt.Errorf("wire land orchestrator: %w", err)
	}
	landSvc, err := service.NewLandService(resolver, channel, mainIdx, parents, attestor, orch, policy)
	if err != nil {
		_ = closer()
		return nil, nil, fmt.Errorf("wire land service: %w", err)
	}
	return landAdapter{svc: landSvc}, closer, nil
}

// landAdapter adapts service.LandService's result to the daemon's
// SandboxLander contract.
type landAdapter struct{ svc *service.LandService }

// Land routes a task land through the verified land service.
func (a landAdapter) Land(ctx context.Context, taskID string) (int, string, error) {
	s, err := a.svc.Land(ctx, taskID)
	if err != nil {
		return 0, "", err
	}
	return s.Commits, s.Branch, nil
}

// loadOrGenerateAttestor loads the host attestation key, generating it once
// (host-side, 0600, never in a guest) when absent so the land path is
// self-bootstrapping on first use. The same service both issues attestations
// (host-side, SEC-01) and verifies them on the land gate. Refs: FR-17.6, FR-17.38, SEC-01
func loadOrGenerateAttestor(hostRoot string, clock func() time.Time, logger *slog.Logger) (*attest.Service, error) {
	if s, err := attest.NewService(hostRoot, clock); err == nil {
		return s, nil
	}
	if err := attest.GenerateKey(context.Background(), hostRoot, slogKeyAuditor{logger: logger}); err != nil {
		return nil, fmt.Errorf("generate attestation key: %w", err)
	}
	s, err := attest.NewService(hostRoot, clock)
	if err != nil {
		return nil, fmt.Errorf("load attestation key after generate: %w", err)
	}
	return s, nil
}

// slogKeyAuditor records attestation-key lifecycle events in the daemon log.
type slogKeyAuditor struct{ logger *slog.Logger }

// RecordKeyChange logs one attestation-key change.
func (a slogKeyAuditor) RecordKeyChange(_ context.Context, detail string) error {
	a.logger.Warn("attestation key changed", "event", "attestation_key", "detail", detail)
	return nil
}
