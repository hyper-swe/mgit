//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

/*
#define _GNU_SOURCE
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <libkrun.h>
#if defined(__linux__)
#include <link.h>
#elif defined(__APPLE__)
#include <mach-o/dyld.h>
#endif

typedef struct { char *buf; size_t len, cap; } mgit_names;

// mgit_append adds one name and a newline; 1 on allocation failure.
static int mgit_append(mgit_names *n, const char *name) {
	if (name == NULL || name[0] == '\0') return 0;
	size_t l = strlen(name);
	if (n->len + l + 2 > n->cap) {
		size_t cap = n->cap ? n->cap : 4096;
		while (cap < n->len + l + 2) cap *= 2;
		char *b = realloc(n->buf, cap);
		if (b == NULL) return 1;
		n->buf = b;
		n->cap = cap;
	}
	memcpy(n->buf + n->len, name, l);
	n->len += l;
	n->buf[n->len++] = '\n';
	n->buf[n->len] = '\0';
	return 0;
}

#if defined(__linux__)
static int mgit_collect(struct dl_phdr_info *info, size_t size, void *data) {
	(void)size;
	return mgit_append((mgit_names *)data, info->dlpi_name);
}
#endif

// mgit_loaded_images returns every shared object the dynamic loader has
// mapped into this process, one path per line (caller frees), or NULL.
static char *mgit_loaded_images(void) {
	mgit_names n = {0};
#if defined(__linux__)
	dl_iterate_phdr(mgit_collect, &n);
#elif defined(__APPLE__)
	uint32_t count = _dyld_image_count();
	for (uint32_t i = 0; i < count; i++) {
		if (mgit_append(&n, _dyld_get_image_name(i)) != 0) break;
	}
#endif
	return n.buf;
}
*/
import "C"

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/hyper-swe/mgit/internal/model"
)

// probeInProcess answers the `--vmm` question inside the probe child: make
// libkrun load libkrunfw, then report what the loader mapped and whether the
// networking API is present. Refs: MGIT-229
func probeInProcess() model.VMMReport {
	ctxErr := loadKernelLibrary()
	r := describeLoaded(loadedImages(), newCapabilityProbe().ProbeNetworking(), realBundleCheck().err())
	if ctxErr != nil {
		r.Problems = append(r.Problems, ctxErr.Error())
	}
	return r
}

// loadKernelLibrary creates one libkrun context and frees it at once.
//
// Creating a context is what makes libkrun dlopen libkrunfw (by leaf name,
// from its own code), so it is the only faithful way to ask whether the
// guest kernel library can be found. The context is NEVER started: it cannot
// become a guest, so the NIC funnel's reason (a started context without a
// NIC boots on TSI) does not reach it. The enforcement test pins both facts.
// Refs: MGIT-229, ADR-010
func loadKernelLibrary() error {
	ctx := C.krun_create_ctx()
	if ctx < 0 {
		return fmt.Errorf("libkrun could not create a VM context (krun_create_ctx: error %d), "+
			"so no guest can boot", int(-ctx))
	}
	C.krun_free_ctx(C.uint32_t(ctx))
	return nil
}

// loadedImages lists the shared objects mapped into this process, with each
// path made absolute and free of `..` so a report names a real directory.
func loadedImages() []string {
	cs := C.mgit_loaded_images()
	if cs == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(cs))
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(C.GoString(cs)), "\n") {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, p)
	}
	return out
}
