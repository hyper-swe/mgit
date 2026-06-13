package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ProjectRoot returns the absolute path to the repository root by
// walking up from the caller until it finds go.mod. Shared so tests
// across packages do not each re-implement the walk.
func ProjectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testutil: cannot determine caller for project root")
	}
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("testutil: go.mod not found in any parent directory")
		}
		dir = parent
	}
}
