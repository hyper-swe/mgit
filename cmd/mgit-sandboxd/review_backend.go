package main

import (
	"context"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
)

// reviewBackendName names the kind of backend the daemon selected, for the
// review verb: the container fallback when that was requested, else the
// platform hypervisor's own kind. The review run refuses any kind that does not
// deliver the bundle as a launch-time image.
func reviewBackendName(ctx context.Context, requested string) string {
	if requested == sandboxd.BackendRequestContainer {
		return model.BackendContainer
	}
	return describeHypervisor(ctx).VMM
}
