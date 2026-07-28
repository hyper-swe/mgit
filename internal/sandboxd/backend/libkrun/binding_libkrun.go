//go:build libkrun && cgo

// The real libkrun binding. Built only under the "libkrun" tag so the default
// pure-Go build of mgit and mgit-sandboxd keeps working on hosts that do not
// have libkrun installed (see binding_unavailable.go). Refs: ADR-010
package libkrun

/*
#cgo pkg-config: libkrun
#include <stdlib.h>
#include <libkrun.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// libkrunAPI is the CGO implementation of krunAPI. It is a thin 1:1 mirror of
// the C calls: the ORDER that matters for containment (the NIC first, always)
// lives in guestCtx, in pure Go, so it stays testable on hosts without
// libkrun. Refs: FR-17.7, SEC-04, ADR-010
type libkrunAPI struct{}

// newPlatformAPI reports the real binding. Building under the libkrun tag is
// the declaration that libkrun is present; a missing shared library surfaces
// at link/load time rather than here.
func newPlatformAPI() (krunAPI, error) { return libkrunAPI{}, nil }

// krunErr converts libkrun's negative-errno return into an error, or nil.
func krunErr(op string, rc C.int32_t) error {
	if rc < 0 {
		return fmt.Errorf("%s: libkrun error %d", op, int32(rc))
	}
	return nil
}

// CreateCtx allocates a libkrun configuration context.
func (libkrunAPI) CreateCtx() (uint32, error) {
	rc := C.krun_create_ctx()
	if err := krunErr("krun_create_ctx", rc); err != nil {
		return 0, err
	}
	return uint32(rc), nil
}

// AddNetUnixgram attaches the virtio-net device backed by a host unixgram
// socket. This call is what disables libkrun's TSI fallback, so it is the
// backend's central containment control, not a feature (ADR-010). The host
// end MUST already be bound and draining — libkrun returns success either
// way, but a VM whose backing socket has no peer hangs at boot.
func (libkrunAPI) AddNetUnixgram(ctx uint32, socketPath, mac string) error {
	hw, err := parseMAC(mac)
	if err != nil {
		return err
	}

	cPath := C.CString(socketPath)
	defer C.free(unsafe.Pointer(cPath))

	// krun_add_net_unixgram takes a pointer to six bytes; hw is a Go array
	// that stays alive for the duration of the call.
	cMAC := (*C.uint8_t)(unsafe.Pointer(&hw[0]))

	// fd = -1 selects the path form; no extra virtio-net features or flags.
	rc := C.krun_add_net_unixgram(C.uint32_t(ctx), cPath, C.int(-1), cMAC, 0, 0)
	return krunErr("krun_add_net_unixgram", rc)
}

// SetVMConfig applies the vCPU and memory caps (T7 resource abuse).
func (libkrunAPI) SetVMConfig(ctx uint32, vcpus uint8, ramMiB uint32) error {
	rc := C.krun_set_vm_config(C.uint32_t(ctx), C.uint8_t(vcpus), C.uint32_t(ramMiB))
	return krunErr("krun_set_vm_config", rc)
}

// FreeCtx releases a context that will not be started.
func (libkrunAPI) FreeCtx(ctx uint32) error {
	return krunErr("krun_free_ctx", C.krun_free_ctx(C.uint32_t(ctx)))
}
