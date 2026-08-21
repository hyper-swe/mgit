package guestnet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guest shipped NO /etc/hosts at all, so every localhost lookup fell
// through to DNS — and under the default deny-all egress that lookup dies.
// Measured in a live guest before the fix:
//
//	$ cat /etc/hosts        -> No such file or directory
//	$ getent hosts localhost -> (nothing)
//
// That is not a niche breakage. vitest, vite, jest and anything else binding
// or dialing a local port resolve "localhost" first, so the failure mode is
// "every JS project fails with EAI_AGAIN inside the sandbox" — and it looks
// like a network policy problem, which sends the user to the wrong place
// entirely. Two lines fix it. Refs: MGIT-159, SEC-04, FR-17.7
func TestEnsureHosts_WritesLoopbackWhenTheImageShipsNone(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "hosts")

	require.NoError(t, EnsureHosts(path))

	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, "127.0.0.1\tlocalhost")
	assert.Contains(t, body, "::1\tlocalhost")
	assert.Contains(t, body, "ip6-localhost", "the IPv6 aliases tools look for")
}

// An image that ships its own hosts file has made a decision; mgit adds what
// is missing and rewrites nothing. Refs: MGIT-159
func TestEnsureHosts_PreservesWhatTheImageProvided(t *testing.T) {
	tests := []struct {
		name        string
		existing    string
		wantKept    []string
		wantAdded   []string
		wantNoAdded bool
	}{
		{
			name:        "already_maps_localhost",
			existing:    "127.0.0.1 localhost myhost\n10.0.0.5 registry.internal\n",
			wantKept:    []string{"10.0.0.5 registry.internal", "127.0.0.1 localhost myhost"},
			wantNoAdded: true,
		},
		{
			name:      "has_entries_but_no_localhost",
			existing:  "10.0.0.5 registry.internal\n",
			wantKept:  []string{"10.0.0.5 registry.internal"},
			wantAdded: []string{"127.0.0.1\tlocalhost"},
		},
		{
			name:      "empty_file",
			existing:  "",
			wantAdded: []string{"127.0.0.1\tlocalhost"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hosts")
			require.NoError(t, os.WriteFile(path, []byte(tt.existing), 0o644)) //nolint:gosec // hosts is world-readable by design

			require.NoError(t, EnsureHosts(path))

			got, err := os.ReadFile(path) //nolint:gosec // test temp path
			require.NoError(t, err)
			body := string(got)
			for _, keep := range tt.wantKept {
				assert.Contains(t, body, keep, "an image's own entry was lost")
			}
			for _, add := range tt.wantAdded {
				assert.Contains(t, body, add)
			}
			if tt.wantNoAdded {
				assert.Equal(t, tt.existing, body, "a file that already maps localhost must not be touched at all")
			}
		})
	}
}

// Running twice must not duplicate: a guest may re-init, and a hosts file that
// grows a localhost line per boot is its own defect. Refs: MGIT-159
func TestEnsureHosts_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	require.NoError(t, EnsureHosts(path))
	first, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)

	require.NoError(t, EnsureHosts(path))
	second, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
}

// The file must be world-readable: every resolver in the guest reads it, and
// commands do not necessarily run as the writer. Refs: MGIT-159
func TestEnsureHosts_IsReadableByEveryone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	require.NoError(t, EnsureHosts(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o044, "a hosts file only root can read is not a hosts file")
}
