package libkrun

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// libkrun gates krun_add_net_* behind an OPT-IN build flag (upstream
// `make NET=1`). A libkrun built without it exports none of those symbols,
// and because mgit attaches an explicit NIC in EVERY network mode — with no
// net device libkrun enables TSI and the guest gets full host egress — such a
// build cannot host a sandbox at all.
//
// There is deliberately no degraded mode to fall back to: a NIC-less VM IS
// the leak. So the only correct behavior is to refuse, with a message that
// names the remedy. These tests pin that. Refs: MGIT-61.14, ADR-010, SEC-04

// stubCapability reports a scripted capability result, standing in for a
// libkrun built with or without networking (we cannot ship a broken one).
type stubCapability struct{ err error }

func (s stubCapability) ProbeNetworking() error { return s.err }

func TestRequireNetworking_AbsentCapability_RefusesWithTheRemedy(t *testing.T) {
	probe := stubCapability{err: errors.New("krun_add_net_unixgram not found in the linked libkrun")}

	err := requireNetworking(probe)
	if err == nil {
		t.Fatal("a libkrun without networking must be refused: attaching no NIC is the TSI egress leak")
	}
	if !errors.Is(err, model.ErrSandboxBackendUnavailable) {
		t.Errorf("error %v does not wrap ErrSandboxBackendUnavailable", err)
	}
	// The message must carry the CAUSE and the FIX. A bare missing-symbol
	// error tells an operator nothing about what to do.
	got := strings.ToLower(err.Error())
	for _, want := range []string{"without networking", "net=1", "egress"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRequireNetworking_PresentCapability_Passes(t *testing.T) {
	if err := requireNetworking(stubCapability{}); err != nil {
		t.Fatalf("a libkrun with networking must be accepted: %v", err)
	}
}

func TestRequireNetworking_NoProbe_Passes(t *testing.T) {
	// A build with no binding (and therefore no probe) is already refused by
	// newPlatformAPI with its own actionable message; the capability gate
	// must not turn that into a second, more confusing failure.
	if err := requireNetworking(nil); err != nil {
		t.Fatalf("absent probe must not fail the gate: %v", err)
	}
}

func TestNewHypervisor_RefusesWhenNetworkingIsMissing(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	_, err := newHypervisor(logger, stubCapability{
		err: errors.New("krun_add_net_unixgram not found"),
	})
	if err == nil {
		t.Fatal("the hypervisor must not construct against a libkrun that cannot attach a NIC")
	}
	if !strings.Contains(err.Error(), "NET=1") {
		t.Errorf("error %q does not name the remedy", err)
	}
}

func TestNewHypervisor_LogsTheLinkedVMMAndItsCapabilities(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := newHypervisor(logger, stubCapability{}); err != nil {
		t.Fatalf("newHypervisor: %v", err)
	}
	// An operator must be able to tell from the log WHICH VMM is linked and
	// that its networking capability was actually verified — otherwise a tap
	// that silently drops NET=1 is invisible until a guest leaks.
	out := logged.String()
	for _, want := range []string{"vmm_capabilities", "libkrun", "networking"} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log %q does not mention %q", out, want)
		}
	}
}
