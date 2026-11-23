package main

import (
	"context"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
)

// The review verb needs to know which kind of backend it runs on, because only
// a launch-time-image backend can deliver the bundle without a live share.
func TestReviewBackendName_FollowsTheBackendTheDaemonSelected(t *testing.T) {
	if got := reviewBackendName(context.Background(), sandboxd.BackendRequestContainer); got != model.BackendContainer {
		t.Fatalf("container request reported %q", got)
	}
	got := reviewBackendName(context.Background(), sandboxd.BackendRequestAuto)
	if got == "" || got == model.BackendContainer {
		t.Fatalf("the platform hypervisor reported %q", got)
	}
}
