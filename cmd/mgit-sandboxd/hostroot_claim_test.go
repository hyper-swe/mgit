package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd"
)

// A second daemon for a host root another daemon holds must be refused BEFORE
// it reads the index: the first cut of MGIT-197 claimed the root inside Run,
// after the service wiring had already rehydrated the shared index and
// discarded the holder's live sandbox with a `killed` event — the very act
// the claim exists to prevent. The refusal names the holder. Refs: MGIT-197
func TestRun_HostRootHeldByAnotherDaemon_RefusesBeforeReadingTheIndex(t *testing.T) {
	hostRoot := t.TempDir()
	held, err := sandboxd.ClaimHostRoot(hostRoot, "/elsewhere/d.sock")
	require.NoError(t, err)
	defer held.Release()

	var out, logs bytes.Buffer
	code := run([]string{
		"--socket", filepath.Join(t.TempDir(), "sandboxd.sock"),
		"--host-root", hostRoot,
		"--backend", "container",
	}, &out, &logs)

	require.Equal(t, 2, code)
	assert.Contains(t, logs.String(), "already serves")
	assert.Contains(t, logs.String(), "/elsewhere/d.sock", "the refusal names the holder")
	assert.NotContains(t, logs.String(), "registry_rehydrated", "the index must not be read before the claim")

	t.Run("a_free_root_is_claimed_and_the_daemon_proceeds", func(t *testing.T) {
		free := t.TempDir()
		var out, logs bytes.Buffer
		code := run([]string{
			"--socket", filepath.Join(t.TempDir(), "sandboxd.sock"),
			"--host-root", free,
			"--backend", "container", // refused at backend selection, past the claim
		}, &out, &logs)
		require.Equal(t, 2, code)
		assert.NotContains(t, logs.String(), "already serves")
		_, err := os.Stat(filepath.Join(free, "daemon.lock"))
		assert.NoError(t, err, "the claim leaves its lock file (the claim itself is released on exit)")
	})
}
