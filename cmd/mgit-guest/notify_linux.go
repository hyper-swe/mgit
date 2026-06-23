//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/mdlayher/vsock"
)

// notifyDialTimeout bounds the guest->host notify dial so a missing or slow
// host listener never stalls the guest's exec loop.
const notifyDialTimeout = 3 * time.Second

// emitLandReady signals the HOST that the guest has committed work and is ready
// to land (the auto-land trigger, MGIT-11.10.11). It dials the host vsock
// (vsock.Host == VMADDR_CID_HOST) on notifyPort and closes immediately: the
// notification carries NO land data and asserts NO provenance — it is purely a
// trigger. The host then runs the EXISTING verified host-initiated land pull;
// the guest holds no signing key and never imports objects (SEC-01). It is
// best-effort and idempotent: a redundant trigger drives a host no-op land
// (the host pull derives only NEW commits), so emitting after each completed
// exec is safe. A dial failure (no host listener) is returned for the caller to
// log and ignore. Refs: MGIT-11.10.11, SEC-01, SEC-10
func emitLandReady(notifyPort uint32, logger *slog.Logger) {
	if notifyPort == 0 {
		return // auto-land trigger disabled
	}
	conn, err := vsock.Dial(vsock.Host, notifyPort, nil)
	if err != nil {
		// No host notify listener (e.g. land not wired, or a backend without the
		// guest->host path): the host-initiated `mgit sandbox land` still works.
		logger.Debug("mgit-guest land-ready notify not delivered",
			"event", "notify_undelivered", "error", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	// Bound the close-after-connect so a wedged host cannot pin the goroutine.
	_ = conn.SetDeadline(time.Now().Add(notifyDialTimeout))
	logger.Info("mgit-guest signaled land-ready", "event", "notify_emitted", "host_port", notifyPort)
}

// describeNotify is a tiny helper to keep the serve loop readable; it never
// errors (emit is best-effort).
func describeNotify(notifyPort uint32) string {
	if notifyPort == 0 {
		return "disabled"
	}
	return fmt.Sprintf("host:%d", notifyPort)
}
