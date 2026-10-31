package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// Observe the actual signed lock while the warning is written, not merely
// its final output: a warning after the switch would be too late.
type stalePinObserver struct {
	bytes.Buffer
	host, oldRef string
	warnedBefore bool
}

func (w *stalePinObserver) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "MGIT-286.1") {
		entry, err := images.LookupEntry(w.host, "base")
		w.warnedBefore = err == nil && "base@"+entry.Digest == w.oldRef
	}
	return w.Buffer.Write(p)
}

func TestStaleBase_RecomposeWarnsRecordedWorktreeBeforeSwitch(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	srv, ref, move := movableTagImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=before"})
	defer srv.Close()
	first := composeInto(t, repo, ref)
	oldRef := first["image_ref"].(string)
	host := filepath.Join(repo, ".mgit", "sandbox")
	store, err := index.New(filepath.Join(repo, ".mgit", "sandbox", model.SandboxRegistryFile), time.Now)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	worktree := filepath.Join(t.TempDir(), "task-worktree")
	require.NoError(t, os.MkdirAll(worktree, 0o700))
	reg := &model.SandboxRegistration{
		Info: model.SandboxInfo{ID: "old-base-box", TaskID: "MGIT-286.1", WorktreePath: worktree,
			ImageDigest: first["base_digest"].(string), State: model.StateCreated,
			NetworkMode: model.NetworkModeNone, CreatedAt: time.Now().UTC()},
		ImageRef: oldRef,
	}
	require.NoError(t, store.UpsertSandbox(t.Context(), reg))
	before, err := store.ListSandboxes(t.Context())
	require.NoError(t, err)
	move(map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=after"})
	t.Chdir(repo)
	warning := &stalePinObserver{host: host, oldRef: oldRef}
	cmd := sandboxBaseCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(warning)
	cmd.SetArgs([]string{"from", ref, "--guest-bin-dir", fakeGuestBins(t), "--plain-http", "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.Contains(t, warning.String(), "MGIT-286.1")
	assert.Contains(t, warning.String(), worktree)
	assert.Contains(t, warning.String(), oldRef)
	assert.True(t, warning.warnedBefore, "warning must precede the signed lock switch")
	current, err := images.LookupEntry(host, "base")
	require.NoError(t, err)
	assert.NotEqual(t, first["base_digest"], current.Digest)
	assert.Contains(t, warning.String(), "mgit sandbox remove 'MGIT-286.1' && mgit sandbox launch --task-id 'MGIT-286.1' --worktree ")
	assert.Contains(t, warning.String(), "--worktree '"+worktree+"' --image base@"+current.Digest+"\n")
	// Both printed commands must also be accepted by the real command parser.
	client := &fakeSandboxClient{}
	_, err = runSandbox(okConnect(client), "remove", reg.Info.TaskID)
	require.NoError(t, err)
	_, err = runSandbox(okConnect(client), "launch", "--task-id", reg.Info.TaskID, "--worktree", worktree, "--image", "base@"+current.Digest)
	require.NoError(t, err)
	assert.Equal(t, reg.Info.TaskID, client.removedTID)
	require.NotNil(t, client.launched)
	assert.Equal(t, "base@"+current.Digest, client.launched.ImageRef)
	assert.Equal(t, canonicalPath(worktree), client.launched.WorktreePath)
	kept, err := store.ListSandboxes(t.Context())
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.Equal(t, before[0], kept[0], "warning and recompose must not rewrite the existing registration")
}

func TestStaleBase_OnlyActiveMatchingRegistrationsWarn(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		matching    bool
		warn        bool
	}{
		{"created", model.StateCreated, true, true},
		{"running", model.StateRunning, true, true},
		{"suspended", model.StateSuspended, true, true},
		{"destroyed", model.StateDestroyed, true, false},
		{"dead", model.StateDead, true, false},
		{"landed", model.StateLanded, true, false},
		{"other reference", model.StateCreated, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			_, err := initTrustRoot(t, repo)
			require.NoError(t, err)
			srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=before"})
			defer srv.Close()
			doc := composeInto(t, repo, ref)
			oldRef := doc["image_ref"].(string)
			registeredRef := oldRef
			if !tc.matching {
				registeredRef = "another@" + doc["base_digest"].(string)
			}
			store, err := index.New(filepath.Join(repo, ".mgit", "sandbox", model.SandboxRegistryFile), time.Now)
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			require.NoError(t, store.UpsertSandbox(t.Context(), &model.SandboxRegistration{
				Info: model.SandboxInfo{ID: "recorded", TaskID: "MGIT-286.3", WorktreePath: filepath.Join(repo, "wt"),
					ImageDigest: doc["base_digest"].(string), State: tc.state, NetworkMode: model.NetworkModeNone,
					CreatedAt: time.Now().UTC()}, ImageRef: registeredRef,
			}))
			var out bytes.Buffer
			host := filepath.Join(repo, ".mgit", "sandbox")
			warnStaleBaseUsers(t.Context(), host, "base", "sha256:"+strings.Repeat("c", 64), &out)
			assert.Equal(t, tc.warn, strings.Contains(out.String(), "MGIT-286.3"), out.String())
			out.Reset()
			warnStaleBaseUsers(t.Context(), host, "base", doc["base_digest"].(string), &out)
			assert.Empty(t, out.String(), "unchanged pin requires no warning")
		})
	}
}

func TestStaleBase_UnreadableRegistryIsNotReportedAsUnused(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=before"})
	defer srv.Close()
	doc := composeInto(t, repo, ref)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".mgit", "sandbox", model.SandboxRegistryFile), []byte("not a SQLite database"), 0o600))
	var out bytes.Buffer
	warnStaleBaseUsers(t.Context(), filepath.Join(repo, ".mgit", "sandbox"), "base", "sha256:"+strings.Repeat("d", 64), &out)
	assert.Contains(t, out.String(), "cannot determine")
	assert.Contains(t, out.String(), doc["image_ref"])
	assert.Contains(t, out.String(), "--image base@sha256:"+strings.Repeat("d", 64))
}

// A CLI that only wants to warn must never run a newer daemon's migrations.
func TestStaleBase_RegistryInspectionDoesNotMigrate(t *testing.T) {
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=old"})
	defer srv.Close()
	composeInto(t, repo, ref)
	host := filepath.Join(repo, ".mgit", "sandbox")
	path := filepath.Join(host, model.SandboxRegistryFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL, applied_at TEXT NOT NULL);
 INSERT INTO schema_version VALUES(1, 'legacy');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	before, err := os.ReadFile(path) //nolint:gosec // G304: fixed registry path inside the test-owned repository
	require.NoError(t, err)
	var out bytes.Buffer
	warnStaleBaseUsers(t.Context(), host, "base", "sha256:"+strings.Repeat("e", 64), &out)
	after, err := os.ReadFile(path) //nolint:gosec // G304: same test-owned registry snapshot
	require.NoError(t, err)
	assert.Equal(t, before, after, "warning inspection must not migrate or write the daemon-owned registry")
	assert.Contains(t, out.String(), "cannot determine", "an unsupported schema is unknown, not unused")
}
