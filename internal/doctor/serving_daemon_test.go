package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// THE DAEMON THAT ANSWERS, NOT THE BINARY ON DISK (MGIT-221). An upgrade
// replaces the binaries but leaves a running daemon running, and every
// daemon row read ok while a 0.6.7 daemon answered a 0.6.8 CLI: daemon/loads
// runs the binary on disk. This row compares the version the daemon SERVING
// THIS REPOSITORY recorded when it started with the CLI's own, and states a
// mismatch as a difference naming both, with the pid and the remedy. The
// build stamp is not compared: binaries built in separate jobs of one
// release differ there. Refs: MGIT-221, MGIT-174, R-H300 rule 2
func TestServingDaemonVersionCheck_ComparesTheDaemonThatAnswers(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o750))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(base, link))
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(other, 0o750))
	const cli = "0.6.8 (commit: 4548edd, built: 2026-09-23T05:57:00Z)"
	alive := daemonrec.Status{Alive: true}
	rec := func(pid int, root, version string, st daemonrec.Status) daemonrec.Listed {
		return daemonrec.Listed{Record: daemonrec.Record{PID: pid, RepoRoot: root, Socket: "/s", Version: version}, Status: st}
	}

	tests := []struct {
		name       string
		daemons    []daemonrec.Listed
		wantStatus Status
		wantIn     []string
	}{
		{"the_same_build_answers", []daemonrec.Listed{rec(41, repo, "0.6.8 (commit: 4548edd, built: 2026-09-23T06:10:00Z)", alive)},
			StatusOK, []string{"pid 41", "0.6.8 (commit: 4548edd"}},
		{"an_older_release_answers", []daemonrec.Listed{rec(67010, repo, "0.6.7 (commit: 487e143, built: 2026-09-22T12:00:00Z)", alive)},
			StatusDiffers, []string{"pid 67010", "0.6.7 (commit: 487e143)", "0.6.8 (commit: 4548edd)", "mgit sandbox daemons stop --repo-root " + repo}},
		{"the_same_version_another_commit_answers", []daemonrec.Listed{rec(7, repo, "0.6.8 (commit: 1111111, built: x)", alive)},
			StatusDiffers, []string{"pid 7", "commit: 1111111"}},
		{"a_daemon_that_recorded_no_version", []daemonrec.Listed{rec(9, repo, "", alive)},
			StatusDiffers, []string{"pid 9", "recorded no version"}},
		{"the_repository_reached_through_a_symlink", []daemonrec.Listed{rec(12, filepath.Join(link, "repo"), "0.6.7 (commit: 487e143, built: y)", alive)},
			StatusDiffers, []string{"pid 12", "0.6.7"}},
		{"an_unstamped_daemon_against_a_release_cli", []daemonrec.Listed{rec(21, repo, "dev (commit: none, built: unknown)", alive)},
			StatusDiffers, []string{"pid 21", "dev (commit: none)"}},
		{"only_another_repositorys_daemon_runs", []daemonrec.Listed{rec(5, other, "0.6.7 (commit: 487e143, built: y)", alive)},
			StatusNotChecked, []string{"no daemon serves this repository"}},
		{"only_a_dead_record_for_this_repository", []daemonrec.Listed{rec(6, repo, "0.6.7 (commit: 487e143, built: y)", daemonrec.Status{})},
			StatusNotChecked, []string{"no daemon serves this repository"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ServingDaemonVersionCheck{
				List:     func(context.Context) ([]daemonrec.Listed, error) { return tt.daemons, nil },
				RepoRoot: func() (string, error) { return repo, nil },
				CLI:      cli,
			}
			got := c.Run(context.Background())
			assert.Equal(t, "daemon/serving-version", got.Name)
			assert.Equal(t, "MGIT-221", got.Incident)
			assert.Equal(t, tt.wantStatus, got.Status, "%s | %s", got.Summary, got.Remedy)
			for _, w := range tt.wantIn {
				assert.Contains(t, got.Summary+" "+got.Remedy+" "+got.Reason, w)
			}
		})
	}
}

// A build with neither -ldflags nor version-control information reports
// commit "none", and two such builds look identical whatever they were built
// from. Against such a CLI, a daemon at the same version token cannot be
// told apart: that is stated as not-checked with the reason, never ok. A
// different version token is still a difference. Refs: MGIT-221
func TestServingDaemonVersionCheck_AnUnstampedBuildCannotBeToldApart(t *testing.T) {
	repo := t.TempDir()
	alive := daemonrec.Status{Alive: true}
	tests := []struct {
		name, cli, daemon string
		want              Status
		wantIn            string
	}{
		{"both_unstamped", "dev (commit: none, built: unknown)", "dev (commit: none, built: unknown)", StatusNotChecked, "no build stamp"},
		{"the_daemon_unstamped", "dev (commit: 65606a5, built: x)", "dev (commit: none, built: unknown)", StatusNotChecked, "no build stamp"},
		{"the_cli_unstamped", "dev (commit: none, built: unknown)", "dev (commit: 65606a5, built: x)", StatusNotChecked, "no build stamp"},
		{"an_unstamped_cli_and_a_release_daemon", "dev (commit: none, built: unknown)", "0.6.7 (commit: 487e143, built: y)", StatusDiffers, "0.6.7 (commit: 487e143)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ServingDaemonVersionCheck{
				List: func(context.Context) ([]daemonrec.Listed, error) {
					return []daemonrec.Listed{{Record: daemonrec.Record{PID: 3, RepoRoot: repo, Version: tt.daemon}, Status: alive}}, nil
				},
				RepoRoot: func() (string, error) { return repo, nil },
				CLI:      tt.cli,
			}
			got := c.Run(context.Background())
			assert.Equal(t, tt.want, got.Status, got.Summary)
			assert.Contains(t, got.Summary+" "+got.Reason, tt.wantIn)
		})
	}
}

// What the check could not read is not-checked with its reason, never ok.
func TestServingDaemonVersionCheck_WhatItCannotReadIsNotChecked(t *testing.T) {
	tests := []struct {
		name string
		c    ServingDaemonVersionCheck
		want string
	}{
		{"the_records", ServingDaemonVersionCheck{
			List:     func(context.Context) ([]daemonrec.Listed, error) { return nil, errors.New("permission denied") },
			RepoRoot: func() (string, error) { return "/r", nil }, CLI: "x"}, "permission denied"},
		{"the_repository", ServingDaemonVersionCheck{
			List:     func(context.Context) ([]daemonrec.Listed, error) { return nil, nil },
			RepoRoot: func() (string, error) { return "", errors.New("not an mgit repository") }, CLI: "x"}, "not an mgit repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.c.Run(context.Background())
			assert.Equal(t, StatusNotChecked, got.Status)
			assert.Contains(t, got.Reason, tt.want)
		})
	}
}
