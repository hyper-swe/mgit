package main

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// hypervisorDeps are the inputs every platform microVM backend needs.
// The per-platform newHypervisorBackend (build-tagged) turns these into
// the concrete SandboxManager: firecracker on Linux, vzf on macOS, and a
// graceful "unavailable" on every other OS (Windows included) until the
// WCOW backend lands (ADR-006, MGIT-12). The struct is OS-neutral so the
// future Windows backend slots in with no caller change.
type hypervisorDeps struct {
	hostRoot string // host config root holding images.lock + trust root (FR-17.13)
	workDir  string // sandbox-local state root; never a worktree
	logger   *slog.Logger
	clock    func() time.Time
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
