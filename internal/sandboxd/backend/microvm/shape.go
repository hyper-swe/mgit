package microvm

import (
	"fmt"
	"os"

	"github.com/hyper-swe/mgit/internal/model"
)

// checkBootShape refuses, before anything is created, a resolved image this
// backend cannot boot. Each backend boots one shape: firecracker and vzf a
// kernel plus an ext4 rootfs image, libkrun a directory (libkrunfw supplies
// its kernel). A root that cannot be stat'ed is left to the launch to report,
// which names the path; this check claims only what it can see. Other
// backends are not judged here. Refs: MGIT-233, MGIT-230.4
func checkBootShape(backend, imageRef string, images ImagePaths) error {
	info, err := os.Stat(images.RootfsPath)
	if err != nil {
		return nil //nolint:nilerr // cannot tell: the launch reports the missing root with its path
	}
	switch backend {
	case model.BackendLibkrun:
		if !info.IsDir() {
			return fmt.Errorf("%w: the libkrun backend boots a directory guest root (libkrunfw supplies the "+
				"kernel), but guest base %s is a kernel + rootfs image (%s); compose a directory base with "+
				"`mgit sandbox base from`", model.ErrGuestBaseUnbootable, imageRef, images.RootfsPath)
		}
	case model.BackendKVM, model.BackendVZF:
		shape := ""
		switch {
		case info.IsDir():
			shape = "is a directory"
		case images.KernelPath == "":
			shape = "has a rootfs but no kernel"
		}
		if shape != "" {
			return fmt.Errorf("%w: the %s backend boots a kernel + ext4 rootfs image, but guest base %s %s "+
				"(%s); install an image it can boot with `mgit sandbox image install --from <dir-or-url>`%s",
				model.ErrGuestBaseUnbootable, backendName(backend), imageRef, shape, images.RootfsPath,
				libkrunAlternative(backend))
		}
	}
	return nil
}

// backendName names a backend the way a reader knows it.
func backendName(backend string) string {
	switch backend {
	case model.BackendKVM:
		return "kvm (firecracker)"
	case model.BackendVZF:
		return "vzf (Virtualization.framework)"
	}
	return backend
}

// libkrunAlternative is the other way out on a host where a libkrun daemon
// ships: its release archive boots the directory base as it is.
func libkrunAlternative(backend string) string {
	if backend == model.BackendKVM {
		return ", or run the Linux release archive's daemon, which links libkrun and boots a directory base"
	}
	return ", or use the release's daemon, which links libkrun and boots a directory base"
}
