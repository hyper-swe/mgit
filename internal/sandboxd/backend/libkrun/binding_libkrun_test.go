//go:build libkrun && cgo

package libkrun

import (
	"path/filepath"
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

	// "none" mode needs its host end bound and draining before the VM would
	// boot; binding it here is also what makes the add realistic.
	deny, err := bindDiscardSocket(filepath.Join(dir, denySocketName))
	if err != nil {
		t.Fatalf("bind deny socket: %v", err)
	}
	defer deny.Close()

	cfg := baseCfg(model.NetworkModeNone)
	gc, err := newGuestCtx(api, cfg, dir)
	if err != nil {
		t.Fatalf("newGuestCtx against real libkrun: %v", err)
	}
	// Nothing will start this context; release it.
	if err := gc.api.FreeCtx(gc.id); err != nil {
		t.Errorf("FreeCtx: %v", err)
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
