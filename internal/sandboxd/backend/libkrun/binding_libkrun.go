//go:build libkrun && cgo

// The real libkrun binding. Built only under the "libkrun" tag so the default
// pure-Go build of mgit and mgit-sandboxd keeps working on hosts that do not
// have libkrun installed (see binding_unavailable.go). Refs: ADR-010
package libkrun

/*
#cgo pkg-config: libkrun
#include <errno.h>
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

// AddVirtiofs shares a host directory into the guest under a tag.
//
// The 4th krun_add_virtiofs3 argument is shm_size, the DAX window: a shared
// memory region the guest maps file pages through instead of round-tripping
// every read over the virtio queue. virtiofsDAXWindow selects it; 0 disables
// DAX, which is what this backend shipped with until it was measured.
// Refs: ADR-010 Gate 2
func (libkrunAPI) AddVirtiofs(ctx uint32, tag, hostDir string, readOnly bool) error {
	cTag := C.CString(tag)
	defer C.free(unsafe.Pointer(cTag))
	cDir := C.CString(hostDir)
	defer C.free(unsafe.Pointer(cDir))
	rc := C.krun_add_virtiofs3(C.uint32_t(ctx), cTag, cDir,
		C.uint64_t(virtiofsDAXWindow()), C.bool(readOnly))
	return krunErr("krun_add_virtiofs3", rc)
}

// SetWorkdir sets the workload's working directory, guest-root-relative.
func (libkrunAPI) SetWorkdir(ctx uint32, dir string) error {
	cDir := C.CString(dir)
	defer C.free(unsafe.Pointer(cDir))
	return krunErr("krun_set_workdir", C.krun_set_workdir(C.uint32_t(ctx), cDir))
}

// EnsureVsock makes a vsock device exist with TSI hijacking disabled
// (tsi_features=0: a plain vsock, no socket impersonation).
//
// EEXIST is SUCCESS, not a failure: it means libkrun already created an
// implicit vsock device, which is the state we are asking for. That is the
// macOS case — libkrun pre-creates a TSI vsock at context creation — whereas
// on Linux, once our explicit NIC has disabled TSI, no device is created at
// all and vsock port adds fail ENODEV without this call. Refs: ADR-010
func (libkrunAPI) EnsureVsock(ctx uint32) error {
	rc := C.krun_add_vsock(C.uint32_t(ctx), 0)
	if int32(rc) == -C.EEXIST {
		return nil
	}
	return krunErr("krun_add_vsock", rc)
}

// AddVsockPort maps a guest vsock port to a host unix socket path
// (krun_add_vsock_port2). hostInitiates=true has libkrun LISTEN on the path
// and forward inbound connections to the guest port; false has libkrun
// CONNECT to a daemon-owned listener when the guest dials out.
func (libkrunAPI) AddVsockPort(ctx uint32, port uint32, socketPath string, hostInitiates bool) error {
	cPath := C.CString(socketPath)
	defer C.free(unsafe.Pointer(cPath))
	rc := C.krun_add_vsock_port2(C.uint32_t(ctx), C.uint32_t(port), cPath, C.bool(hostInitiates))
	return krunErr("krun_add_vsock_port2", rc)
}

// SetExec sets the guest PID-1 workload. args are the arguments ONLY —
// libkrun PREPENDS the executable to argv (measured; ADR-010), so passing
// argv[0] here would shift every guest argument by one. env is ALWAYS passed
// as a real (NULL-terminated) array, never NULL: libkrun's NULL-envp
// convenience collects the calling process's environment, which would leak
// daemon state into the guest (SEC-05).
func (libkrunAPI) SetExec(ctx uint32, path string, args, env []string) error {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	cArgs, freeArgs := cStringArray(args)
	defer freeArgs()
	cEnv, freeEnv := cStringArray(env)
	defer freeEnv()
	rc := C.krun_set_exec(C.uint32_t(ctx), cPath, &cArgs[0], &cEnv[0])
	return krunErr("krun_set_exec", rc)
}

// StartEnter boots the VM. On success it NEVER RETURNS: libkrun assumes
// control of the process (stdio included) and exit()s with the guest's exit
// code at shutdown (ADR-010). A return value is always a pre-boot failure.
func (libkrunAPI) StartEnter(ctx uint32) error {
	return krunErr("krun_start_enter", C.krun_start_enter(C.uint32_t(ctx)))
}

// cStringArray converts a Go string slice to a NULL-terminated C array. The
// returned free func releases every element; the array itself is Go memory
// pinned for the duration of the enclosing CGO call.
func cStringArray(ss []string) ([]*C.char, func()) {
	out := make([]*C.char, len(ss)+1) // +1: NULL terminator
	for i, s := range ss {
		out[i] = C.CString(s)
	}
	free := func() {
		for _, p := range out {
			if p != nil {
				C.free(unsafe.Pointer(p))
			}
		}
	}
	return out, free
}

// FreeCtx releases a context that will not be started.
func (libkrunAPI) FreeCtx(ctx uint32) error {
	return krunErr("krun_free_ctx", C.krun_free_ctx(C.uint32_t(ctx)))
}
