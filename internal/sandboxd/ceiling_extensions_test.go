package sandboxd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// checkingManager implements the backend's optional registration checks.
type checkingManager struct {
	launchingManager
	networkErr, layoutErr error
}

func (m *checkingManager) SupportsNetworkMode(string) error { return m.networkErr }
func (m *checkingManager) CheckWorktreeLayout(string) error { return m.layoutErr }

// THE WRAPPER MUST NOT HIDE THE BACKEND'S CHECKS (MGIT-251). The daemon hands
// the service a CeilingManager, and the service asks its manager for optional
// checks by type assertion. A wrapper that implements only the verbs answers
// "not supported" for every one of them, so MGIT-111's registration-time
// network refusal never ran in a real daemon, and MGIT-222's layout refusal
// would not have either. The wrapper forwards each check to the backend it
// wraps, and says nothing when that backend has no such check.
// Refs: MGIT-251, MGIT-111, MGIT-222, MGIT-174
func TestCeilingManager_ForwardsTheBackendsOptionalChecks(t *testing.T) {
	notEnforced := errors.New("allowlist is not enforceable here")
	reachable := errors.New("the store is inside the worktree")
	checked := NewCeilingManager(&checkingManager{launchingManager: launchingManager{fakeManager: *newFakeManager()},
		networkErr: notEnforced, layoutErr: reachable}, 2, 0, 0)
	plain := NewCeilingManager(&launchingManager{fakeManager: *newFakeManager()}, 2, 0, 0)

	var wrapped model.SandboxManager = checked
	enforcer, ok := wrapped.(model.NetworkModeEnforcer)
	require.True(t, ok, "the wrapper answers the network-mode question")
	assert.ErrorIs(t, enforcer.SupportsNetworkMode(model.NetworkModeAllowlist), notEnforced, "with the backend's own answer")
	layout, ok := wrapped.(model.WorktreeLayoutChecker)
	require.True(t, ok, "the wrapper answers the layout question")
	assert.ErrorIs(t, layout.CheckWorktreeLayout("/w"), reachable, "with the backend's own answer")

	assert.NoError(t, plain.SupportsNetworkMode(model.NetworkModeAllowlist), "a backend without the check has no objection")
	assert.NoError(t, plain.CheckWorktreeLayout("/w"), "a backend without the check has no objection")
}

// The case list comes from the SERVICE's source, which the wrapper does not
// control: every optional extension the service type-asserts on its manager
// must be one the wrapper forwards, so the next extension cannot be added to
// the service and forgotten here. Refs: MGIT-251
func TestCeilingManager_ForwardsEveryExtensionTheServiceAsksFor(t *testing.T) {
	// Three of these were forwarded by hand, each when its feature landed;
	// the fourth, the network-mode check, was not, and no test noticed.
	known := map[string]reflect.Type{
		"ArtifactExporter":      reflect.TypeOf((*model.ArtifactExporter)(nil)).Elem(),
		"GuestViewVerifier":     reflect.TypeOf((*model.GuestViewVerifier)(nil)).Elem(),
		"WorktreeSyncer":        reflect.TypeOf((*model.WorktreeSyncer)(nil)).Elem(),
		"NetworkModeEnforcer":   reflect.TypeOf((*model.NetworkModeEnforcer)(nil)).Elem(),
		"WorktreeLayoutChecker": reflect.TypeOf((*model.WorktreeLayoutChecker)(nil)).Elem(),
	}
	files, err := filepath.Glob(filepath.Join("..", "service", "*.go"))
	require.NoError(t, err)
	asked := map[string]bool{}
	assertion := regexp.MustCompile(`s\.manager\.\(model\.(\w+)\)`)
	for _, f := range files {
		if filepath.Ext(f) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		raw, err := os.ReadFile(f) //nolint:gosec // G304: test-only; the service package's own sources
		require.NoError(t, err)
		for _, m := range assertion.FindAllStringSubmatch(string(raw), -1) {
			asked[m[1]] = true
		}
	}
	require.NotEmpty(t, asked, "the service asks its manager for optional extensions; an empty scan means the pattern drifted")
	names := make([]string, 0, len(asked))
	for n := range asked {
		names = append(names, n)
	}
	sort.Strings(names)
	ceiling := reflect.TypeOf((*CeilingManager)(nil))
	for _, n := range names {
		iface, ok := known[n]
		if !assert.True(t, ok, "the service asks for model.%s: add it to this test and forward it from CeilingManager", n) {
			continue
		}
		assert.True(t, ceiling.Implements(iface), "CeilingManager does not forward model.%s, so the service never sees it", n)
	}
}
