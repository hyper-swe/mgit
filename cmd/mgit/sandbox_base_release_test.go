package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// pinRecordTo makes this build vouch for the image a fixture registry serves
// under ref (a tag reference): the record is the tag plus the digest the
// registry resolves it to now, exactly what scripts/release/pin-guest-base.sh
// writes for a real release.
func pinRecordTo(t *testing.T, repo, ref string) guestbase.ReleaseBase {
	t.Helper()
	t.Chdir(repo)
	out, err := runBase(t, repo, "resolve", ref, "--plain-http")
	require.NoError(t, err, "base resolve: %s", out)
	resolved := strings.TrimSpace(out)
	rec := guestbase.ReleaseBase{Image: guestbase.SourceTag(resolved), Digest: guestbase.SourceDigest(resolved)}
	require.NotEmpty(t, rec.Digest, "resolve printed no digest: %q", out)
	prev := releaseBaseRecord
	releaseBaseRecord = func() (guestbase.ReleaseBase, error) { return rec, nil }
	t.Cleanup(func() { releaseBaseRecord = prev })
	return rec
}

// `sandbox base resolve <ref>` PRINTS WHAT A TAG POINTS AT NOW, without
// composing anything: the pin script and the release preflight read it, and
// so can a person asking whether upstream moved. Refs: MGIT-219
func TestSandboxBaseResolve_PrintsWhatATagPointsAtNow(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)

	out, err := runBase(t, repo, "resolve", ref, "--plain-http")
	require.NoError(t, err, "base resolve: %s", out)
	line := strings.TrimSpace(out)
	assert.Regexp(t, `@sha256:[0-9a-f]{64}$`, line, "the resolved reference ends in the manifest digest")
	assert.True(t, strings.HasPrefix(line, ref+"@") || strings.Contains(line, "acme/base:v1@"), "the tag is kept beside the digest: %q", line)

	// Resolving BY DIGEST answers with the same reference: a pinned record
	// still resolves after the tag has moved on.
	byDigest, err := runBase(t, repo, "resolve", guestbase.SourceTag(line)+"@"+guestbase.SourceDigest(line), "--plain-http")
	require.NoError(t, err, "resolve by digest: %s", byDigest)
	assert.Equal(t, guestbase.SourceDigest(line), guestbase.SourceDigest(strings.TrimSpace(byDigest)))
	assert.Empty(t, cachedBaseEntries(t, repo), "resolve composes nothing")
}

// `sandbox base from` WITH NO REFERENCE COMPOSES THE RELEASE'S RECORDED BASE,
// pulling by digest: every host that recomposes "under this release" gets the
// bytes the release was smoke-tested with, whatever the tag points at now.
// Refs: MGIT-219
func TestSandboxBaseFrom_NoReference_ComposesTheReleaseBaseByDigest(t *testing.T) {
	srv, ref := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian"})
	defer srv.Close()
	repo := newRepo(t)
	_, err := initTrustRoot(t, repo)
	require.NoError(t, err)
	rec := pinRecordTo(t, repo, ref)

	out, err := runBase(t, repo, "from", "--guest-bin-dir", fakeGuestBins(t), "--plain-http")
	require.NoError(t, err, "base from (no reference): %s", out)
	assert.Contains(t, out, "release", "the output says it composed the release's recorded base")
	assert.Contains(t, out, rec.Digest)

	entry, err := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	require.NoError(t, err)
	assert.Equal(t, rec.Digest, guestbase.SourceDigest(entry.Source), "the lock's source is the record's digest")
	assert.Equal(t, rec.Ref(), entry.Source, "the lock's source is the record, both halves")
}

// TWO LANES AGAINST ONE RECORD: the lane on the recorded base reads ok, the
// lane on another image reads DIFF with both sides named — through the real
// inspection and the real record seam. Refs: MGIT-219
func TestInspectBaseCurrency_ReleaseRow_TellsTheRecordedBaseFromAnother(t *testing.T) {
	srvA, refA := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian\nVERSION=12.6"})
	defer srvA.Close()
	srvB, refB := fakeImageServer(t, map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian\nVERSION=12.7"})
	defer srvB.Close()
	repoA, repoB := newRepo(t), newRepo(t)
	rec := pinRecordTo(t, repoA, refA)

	compose := func(repo, ref string) doctor.BaseIdentity {
		_, err := initTrustRoot(t, repo)
		require.NoError(t, err)
		out, err := runBase(t, repo, "from", ref, "--guest-bin-dir", fakeGuestBins(t), "--plain-http")
		require.NoError(t, err, "base from: %s", out)
		t.Chdir(repo)
		id, err := inspectBaseCurrency()
		require.NoError(t, err)
		return id
	}
	laneA := compose(repoA, refA)
	laneB := compose(repoB, refB)
	assert.Equal(t, rec.Ref(), laneA.ReleaseRef, "the inspection carries the record")

	row := func(id doctor.BaseIdentity) doctor.Result {
		return doctor.BaseReleaseCheck{Inspect: func() (doctor.BaseIdentity, error) { return id, nil }}.Run(context.Background())
	}
	rowA, rowB := row(laneA), row(laneB)
	assert.Equal(t, doctor.StatusOK, rowA.Status, rowA.Summary)
	assert.Equal(t, doctor.StatusDiffers, rowB.Status, rowB.Summary)
	assert.Contains(t, rowB.Summary, guestbase.SourceDigest(laneB.SourceRef))
	assert.Contains(t, rowB.Summary, rec.Digest)
}

// cachedBaseEntries lists the names registered in the repo's images.lock, or
// nothing when the lock does not exist yet.
func cachedBaseEntries(t *testing.T, repo string) []string {
	t.Helper()
	_, err := images.LookupEntry(filepath.Join(repo, ".mgit", "sandbox"), defaultGuestBaseName)
	if err != nil {
		return nil
	}
	return []string{defaultGuestBaseName}
}
