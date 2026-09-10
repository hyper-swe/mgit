package packaging

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The inventory's `uses:` matcher must match a STEP KEY, not the substring
// inside `statuses:`. The verdict gate's `permissions: statuses: write` was
// read as an action named "write" and reported BARE, failing the Test job
// of the PR that introduced it (MGIT-201). Refs: MGIT-201, MGIT-143
func TestFetchInventory_DoesNotReadStatusesWriteAsAnAction(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("bash", filepath.Join(root, "scripts", "ci", "fetch-inventory.sh"), "--list").CombinedOutput() //nolint:gosec // a fixed script in this repository
	require.NoError(t, err, "inventory --list: %s", out)
	for _, line := range strings.Split(string(out), "\n") {
		assert.NotContains(t, line, "uses: write", "a `statuses: write` permission line was read as an action: %s", line)
	}
}
