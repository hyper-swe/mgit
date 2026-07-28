//go:build !libkrun || !cgo

package libkrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

// A build without the libkrun tag must say so actionably rather than failing
// later with a link or load error. Refs: FR-17.15, ADR-010
func TestNewPlatformAPI_WithoutTheLibkrunTag_IsUnavailableWithRemedy(t *testing.T) {
	api, err := newPlatformAPI()
	if err == nil {
		t.Fatal("expected an error in a build without the libkrun tag")
	}
	if api != nil {
		t.Error("an API was returned by a build that cannot drive libkrun")
	}
	if !errors.Is(err, model.ErrSandboxBackendUnavailable) {
		t.Errorf("error = %v, want it to wrap ErrSandboxBackendUnavailable", err)
	}
	if !strings.Contains(err.Error(), "-tags libkrun") {
		t.Errorf("error %q does not tell the operator how to fix it", err)
	}
}
