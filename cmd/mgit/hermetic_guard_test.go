package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
)

// TestMain pins this package's tests away from the machine they run on: a
// working directory with no mgit repository above it, and a scratch guest-base
// cache. Without that, a test that touched the sandbox found the developer's own
// repository (the walk upward from the package directory reaches the checkout)
// and once moved its guest base. TestMain is a setup step with no assertion of its
// own, so deleting a line of it would go unnoticed until a test mutated a real
// repository. This test is the assertion: it fails if the guarantee regresses.
// Refs: MGIT-150, MGIT-147, MGIT-57
func TestHermetic_NoRepositoryIsReachableFromTheWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	// Nothing above the working directory is an mgit repository, so the walk
	// that sandbox commands do (sandboxRepoRoot) cannot find one.
	for dir := cwd; ; dir = filepath.Dir(dir) {
		_, statErr := os.Stat(filepath.Join(dir, ".mgit"))
		assert.True(t, os.IsNotExist(statErr), "%s holds a .mgit above the test working directory %s", dir, cwd)
		if filepath.Dir(dir) == dir {
			break
		}
	}
	_, err = sandboxRepoRoot(cwd)
	assert.Error(t, err, "a sandbox command run from the test working directory must not find a repository")

	// The working directory is not inside the source checkout either.
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	checkout := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	rel, relErr := filepath.Rel(canonicalPath(checkout), canonicalPath(cwd))
	assert.True(t, relErr != nil || strings.HasPrefix(rel, ".."), "the test working directory %s is inside the checkout %s", cwd, checkout)

	// The guest-base cache is a scratch one, never the machine's.
	root := os.Getenv(basecache.EnvRoot)
	require.NotEmpty(t, root, "the guest-base cache must be redirected for every test in this package")
	// the cache dir may not exist yet, so its parent is the part that is resolved
	resolvedRoot := filepath.Join(canonicalPath(filepath.Dir(root)), filepath.Base(root))
	assert.True(t, strings.HasPrefix(resolvedRoot, canonicalPath(cwd)), "the cache root %s is not under the scratch dir %s", root, cwd)
}
