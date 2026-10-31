package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// Exercise the real CLI process, registry pull and signed-lock replacement.
// The tiny guest binaries are composition fixtures, not a bootable VM image.
func TestStaleBase_RealCLIRecomposeAndRefusal(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mgit")
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".") //nolint:gosec // G204: fixed Go build into a test-owned output path
	build.Dir = filepath.Dir(source)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%s", out)
	repo := newRepo(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // G204: test-built CLI and test-authored argv
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	run("sandbox", "image", "init")
	srv, ref, move := movableTagImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=one"})
	defer srv.Close()
	bins := fakeGuestBins(t)
	compose := func() (string, map[string]any) {
		t.Helper()
		out := run("sandbox", "base", "from", ref, "--plain-http", "--guest-bin-dir", bins, "--json")
		_, body, ok := strings.Cut(out, "{")
		require.True(t, ok, out)
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte("{"+body), &doc), out)
		return out, doc
	}
	_, first := compose()
	oldRef := first["image_ref"].(string)
	store, err := index.New(filepath.Join(repo, ".mgit", "sandbox", model.SandboxRegistryFile), time.Now)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	require.NoError(t, store.UpsertSandbox(t.Context(), &model.SandboxRegistration{
		Info: model.SandboxInfo{ID: "cli-pin", TaskID: "MGIT-286.2", WorktreePath: filepath.Join(repo, "wt"),
			ImageDigest: first["base_digest"].(string), State: model.StateRunning, NetworkMode: model.NetworkModeNone,
			CreatedAt: time.Now().UTC()}, ImageRef: oldRef,
	}))
	move(map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=two"})
	warning, second := compose()
	currentRef := second["image_ref"].(string)
	require.NotEqual(t, oldRef, currentRef)
	assert.Contains(t, warning, "MGIT-286.2")
	assert.Contains(t, warning, oldRef)
	assert.Contains(t, warning, "--image "+currentRef)
	assert.Contains(t, warning, "already running VM is not restarted")
	resolver, err := images.NewStoreWithBaseCache(filepath.Join(repo, ".mgit", "sandbox"), time.Now, testBaseCache(t))
	require.NoError(t, err)
	_, err = resolver.Resolve(oldRef)
	require.ErrorIs(t, err, model.ErrVerificationFailed)
	assert.Contains(t, err.Error(), "--image "+currentRef)
	_, err = resolver.Resolve(currentRef)
	require.NoError(t, err)
	kept, err := store.ListSandboxes(t.Context())
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.Equal(t, model.StateRunning, kept[0].Info.State)
	assert.Equal(t, oldRef, kept[0].ImageRef)
}
