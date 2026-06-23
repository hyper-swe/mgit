package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/container"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/sandboxd/provision"
)

// backendSelection is the resolved daemon configuration needed to pick
// and construct a sandbox backend.
type backendSelection struct {
	backend    string // sandboxd.BackendRequestAuto | BackendRequestContainer
	ackReduced bool   // --acknowledge-reduced-isolation
	hostRoot   string
	repoRoot   string // mgit repo root whose .mgit is the shared store (SEC-03 provisioning)
	workDir    string
	logger     *slog.Logger
	clock      func() time.Time
	peerBinder microvm.PeerBinder // channel peer-identity binder (SEC-10)
}

// selectManager resolves the sandbox backend: the build-tagged platform
// hypervisor for "auto", or the audited reduced-isolation container
// fallback when explicitly requested. A missing hypervisor is a hard
// failure, never a silent downgrade (FR-17.15).
//
// It also returns the active backend's host LAND dialer (firecracker on
// Linux, vzf on macOS) so the daemon land wiring selects the transport by
// active backend rather than hardcoding firecracker. The container fallback
// and any other-OS backend report a nil land dialer (land not served over
// them); the daemon then logs "land not wired" rather than half-serving.
// Refs: FR-17.5, FR-17.15, FR-17.16, MGIT-13.1.1
func selectManager(sel backendSelection) (model.SandboxManager, microvm.GuestDialer, error) {
	var landDialer microvm.GuestDialer
	mgr, err := sandboxd.SelectBackend(context.Background(), sandboxd.SelectOptions{
		Backend:                     sel.backend,
		AcknowledgeReducedIsolation: sel.ackReduced,
		Audit:                       slogBackendAuditor{logger: sel.logger},
	}, sandboxd.BackendFactories{
		// firecracker on Linux, vzf on macOS, graceful "unavailable" on
		// every other OS (Windows runs core mgit without the sandbox in
		// v1 — ADR-006, FR-17.39). Selection is build-tagged in
		// platform_backend_*.go so the future WCOW backend slots in with
		// no change here. The chosen backend's host LAND dialer is captured
		// here for the daemon land wiring.
		Hypervisor: func() (model.SandboxManager, error) {
			m, ld, herr := newHypervisorBackend(hypervisorDeps{
				hostRoot:   sel.hostRoot,
				repoRoot:   sel.repoRoot,
				workDir:    sel.workDir,
				logger:     sel.logger,
				clock:      sel.clock,
				peerBinder: sel.peerBinder,
			})
			landDialer = ld
			return m, herr
		},
		Container: func() (model.SandboxManager, error) {
			// SEC-03 fail-closed: the reduced-isolation container fallback still
			// quarantines the store, so it refuses to construct without a shared
			// store to seed the per-task private store from — never a container
			// bind-mounting the raw worktree with its own store exposed.
			// Refs: SEC-03, MGIT-11.6.9
			prov, err := newStoreProvisioner(hypervisorDeps{hostRoot: sel.hostRoot, repoRoot: sel.repoRoot})
			if err != nil {
				return nil, err
			}
			return container.NewManager(container.Config{
				Runner:           container.PodmanRunner{},
				SensitivePaths:   model.DefaultSandboxPolicy().SensitivePaths,
				StoreProvisioner: prov,
				Logger:           sel.logger,
				Clock:            sel.clock,
			})
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return mgr, landDialer, nil
}

// resolveRepoRoot returns the mgit repo root for SEC-03 provisioning: the
// explicit --repo-root, else the conventional parent of the host config root
// (<repo>/.mgit/sandbox -> <repo>). Mirrors buildLandService's fallback.
// Shared by every backend's fail-closed wiring (firecracker, vzf, container).
func resolveRepoRoot(deps hypervisorDeps) string {
	if deps.repoRoot != "" {
		return deps.repoRoot
	}
	if deps.hostRoot == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(deps.hostRoot))
}

// newStoreProvisioner builds the SEC-03 private-store provisioner from the
// resolved repo root. It is an ERROR (fail closed) when no repo root is known
// or the provisioner cannot be built: the quarantine control cannot be realized
// without a shared store to seed from, and the caller refuses to bring up an
// unquarantined sandbox backend rather than silently degrading. Shared by the
// firecracker (linux), vzf (darwin), and container backend wiring so all three
// fail closed identically. Refs: SEC-03, MGIT-11.6.8, MGIT-11.6.9
func newStoreProvisioner(deps hypervisorDeps) (provision.Provisioner, error) {
	root := resolveRepoRoot(deps)
	if root == "" {
		return nil, fmt.Errorf("%w: SEC-03 quarantine requires a repo root to seed the private store "+
			"(set --repo-root or a host config root); refusing to launch sandboxes unquarantined",
			model.ErrSandboxBackendUnavailable)
	}
	p, err := provision.NewStoreProvisioner(root)
	if err != nil {
		return nil, fmt.Errorf("%w: SEC-03 private-store provisioner: %w", model.ErrSandboxBackendUnavailable, err)
	}
	return p, nil
}

// hypervisorDeps are the inputs every platform microVM backend needs.
// The per-platform newHypervisorBackend (build-tagged) turns these into
// the concrete SandboxManager: firecracker on Linux, vzf on macOS, and a
// graceful "unavailable" on every other OS (Windows included) until the
// WCOW backend lands (ADR-006, MGIT-12). The struct is OS-neutral so the
// future Windows backend slots in with no caller change.
type hypervisorDeps struct {
	hostRoot   string // host config root holding images.lock + trust root (FR-17.13)
	repoRoot   string // mgit repo root whose .mgit is the shared store (SEC-03 provisioning); empty disables
	workDir    string // sandbox-local state root; never a worktree
	logger     *slog.Logger
	clock      func() time.Time
	peerBinder microvm.PeerBinder // channel peer-identity binder (SEC-10); nil disables
}

// newImageResolver returns a verified-image resolver that lazily opens
// the host image store on first use. Opening is deferred so the daemon
// starts even before images.lock and the trust root are configured; a
// launch then fails with a clear error rather than the daemon refusing
// to boot (FR-17.10). The store verifies digest + signature on every
// resolve (FR-17.17, FR-17.29); this only adapts its result to the
// hypervisor-agnostic ImagePaths the backends consume.
func newImageResolver(hostRoot string, clock func() time.Time) func(string) (microvm.ImagePaths, error) {
	var (
		once  sync.Once
		store *images.Store
		err   error
	)
	return func(ref string) (microvm.ImagePaths, error) {
		once.Do(func() { store, err = images.NewStore(hostRoot, clock) })
		if err != nil {
			return microvm.ImagePaths{}, fmt.Errorf("image store unavailable: %w", err)
		}
		resolved, rerr := store.Resolve(ref)
		if rerr != nil {
			return microvm.ImagePaths{}, rerr
		}
		return microvm.ImagePaths{
			KernelPath: resolved.KernelPath,
			RootfsPath: resolved.RootfsPath,
			Cmdline:    resolved.Cmdline,
		}, nil
	}
}
