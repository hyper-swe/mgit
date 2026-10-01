package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// A store's task binding round-trips, and a store without one is unbound.
// Refs: MGIT-256
func TestBoundTaskInStore_RoundTripAndAbsent(t *testing.T) {
	dir := t.TempDir()

	got, err := ReadBoundTaskInStore(dir)
	require.NoError(t, err)
	assert.Empty(t, got, "a store with no record is unbound")

	require.NoError(t, WriteBoundTaskInStore(dir, "MGIT-256.1"))
	got, err = ReadBoundTaskInStore(dir)
	require.NoError(t, err)
	assert.Equal(t, "MGIT-256.1", got)

	data, err := os.ReadFile(filepath.Join(dir, boundTaskFileName)) //nolint:gosec // test-controlled path
	require.NoError(t, err)
	assert.Equal(t, "MGIT-256.1\n", string(data), "the record holds the task ID and nothing else")
}

// An invalid task ID is refused on write, before anything is written.
// Refs: MGIT-256
func TestWriteBoundTaskInStore_InvalidTaskID_Refused(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "not a task", "../etc/passwd"} {
		err := WriteBoundTaskInStore(dir, id)
		require.ErrorIs(t, err, model.ErrInvalidTaskID, "%q", id)
	}
	_, err := os.Stat(filepath.Join(dir, boundTaskFileName))
	assert.True(t, os.IsNotExist(err), "a refused write leaves no record")
}

// A record the store cannot vouch for is an error, never an unbound store: a
// commit must not fall back to another attribution when the binding is
// unreadable. Refs: MGIT-256
func TestReadBoundTaskInStore_UnusableRecord_Error(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"a symlink", func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			require.NoError(t, os.WriteFile(target, []byte("MGIT-1\n"), 0o600))
			require.NoError(t, os.Symlink(target, path))
		}},
		{"a directory", func(t *testing.T, path string) {
			require.NoError(t, os.Mkdir(path, 0o700))
		}},
		{"an invalid task ID", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("not a task\n"), 0o600))
		}},
		{"an empty record", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, nil, 0o600))
		}},
		{"an oversized record", func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("A", maxBoundTaskBytes+1)), 0o600))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.plant(t, filepath.Join(dir, boundTaskFileName))

			got, err := ReadBoundTaskInStore(dir)

			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}
