//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// These exercise the REAL libkrun binding, so they run only in a build that
// linked it (-tags libkrun). Everything else in this package is deliberately
// testable without libkrun; this is the part fakes cannot prove — that the
// actual C calls accept what the pure-Go layer produces.
// Refs: FR-17.7, SEC-04, ADR-010

func TestBinding_ConfiguresARealContextThroughNewGuestCtx(t *testing.T) {
	api, err := newPlatformAPI()
	if err != nil {
		t.Fatalf("libkrun binding unavailable in a -tags libkrun build: %v", err)
	}

	dir := shortTempDir(t)
	// The full production configuration — root virtiofs, worktree share,
	// vsock control ports, guest exec — against the REAL C calls. The host
	// peer is bound by newGuestCtx itself (it owns the NIC's host end).
	spec := baseSpec(model.NetworkModeNone, dir)
	spec.RootDir = testGuestBase(t)
	spec.WorktreePath = t.TempDir()
	spec.WorktreeTag = "work"
	spec.VsockEnabled = true
	spec.ExecArgs = []string{"--vsock-port", "1024"}
	spec.ExecEnv = []string{"PATH=/bin"}

	gc, err := newGuestCtx(api, spec, netDeps{auth: &stubAuthorizer{}})
	if err != nil {
		t.Fatalf("newGuestCtx against real libkrun: %v", err)
	}
	// Nothing will start this context; release it (and its host peer).
	if err := gc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if denySocketExists(dir) {
		t.Error("host peer socket still bound after Close")
	}
}

func TestBinding_RejectsAnUnparsableMACBeforeCallingC(t *testing.T) {
	api, err := newPlatformAPI()
	if err != nil {
		t.Fatalf("libkrun binding unavailable: %v", err)
	}
	// A malformed MAC must fail in Go, not be handed to the C call.
	if err := api.AddNetUnixgram(0, "/tmp/whatever.sock", "not-a-mac"); err == nil {
		t.Fatal("expected a parse error for a malformed MAC")
	}
}

// TestNetCapability_ProbeNetworking_AgreesWithTheLinkedLibrary exercises the
// REAL dlsym(RTLD_DEFAULT, ...) call against whatever libkrun this build
// actually linked, rather than the stubCapability used everywhere else in
// this package (capability_test.go). Every CI/dev libkrun here is required
// to be built with NET=1 (make check-libkrun-net, MGIT-61.14), so the real
// probe must report present.
//
// This is also the regression guard for the build itself: RTLD_DEFAULT is a
// GNU extension glibc's <dlfcn.h> only declares when _GNU_SOURCE is defined
// (Darwin's libc declares it unconditionally), so a build missing that
// #define fails to COMPILE this whole package on Linux with "could not
// determine what C.RTLD_DEFAULT refers to" -- a failure this test's mere
// presence surfaces immediately, on every platform, rather than only when
// something happens to call ProbeNetworking in production. Refs: MGIT-61.13, MGIT-61.14
func TestNetCapability_ProbeNetworking_AgreesWithTheLinkedLibrary(t *testing.T) {
	if err := (netCapability{}).ProbeNetworking(); err != nil {
		t.Fatalf("the linked libkrun must be built with networking (NET=1): %v", err)
	}
}
