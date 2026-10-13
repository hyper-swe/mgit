package microvm

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// FEAT-6.114.1: the guest repository is on by default, and every delivery
// path asks for it the same way.
func TestVMConfig_TheGuestRepoIsOnByDefault(t *testing.T) {
	cfg := vmConfig("id", "/state", model.SandboxLaunchOptions{TaskID: "T-1", WorktreePath: "/w"}, ImagePaths{}, "", "/store")
	assert.False(t, cfg.NoGuestRepo)
	assert.True(t, cfg.GuestRepo(), "libkrun, vzf and firecracker deliver the repo unless told not to")
}

func TestVMConfig_NoGuestRepoTurnsItOff(t *testing.T) {
	cfg := vmConfig("id", "/state", model.SandboxLaunchOptions{TaskID: "T-1", WorktreePath: "/w", NoGuestRepo: true}, ImagePaths{}, "", "/store")
	assert.True(t, cfg.NoGuestRepo)
	assert.False(t, cfg.GuestRepo())
}

// The backends live in packages that need cgo or another OS to build, so
// this reads their delivery calls: each must pass the decision on from the
// launch (cfg.GuestRepo(), or !opts.NoGuestRepo), never a literal, so a
// forced value cannot slip in unseen.
func TestEveryDeliveryPathPassesTheLaunchsDecisionOn(t *testing.T) {
	for path, want := range map[string]string{
		"../libkrun/hypervisor.go":           "staging.BuildForGuest(cfg.WorktreePath, cfg.PrivateStorePath, stagingDir, cfg.GuestRepo())",
		"../vzf/hypervisor_darwin.go":        "staging.BuildForGuest(cfg.WorktreePath, cfg.PrivateStorePath, stagingDir, cfg.GuestRepo())",
		"../firecracker/hypervisor_linux.go": "cfg.PrivateStorePath, 0, cfg.GuestRepo())",
		"../container/manager.go":            "staging.BuildForGuest(opts.WorktreePath, store.Dir, stageDir, !opts.NoGuestRepo)",
	} {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: test-only; fixed sibling paths listed above
		require.NoError(t, err, path)
		assert.True(t, strings.Contains(string(raw), want), "%s must contain %q", path, want)
	}
}
