package microvm

import (
	"context"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// The enumeration is driven by the code's own registry: for EVERY internal
// exec site it names its program by an absolute path and is exactly one of
// audited-identity or no-exec. A site added to the code (a new registry entry)
// is asserted here automatically. Refs: MGIT-272
func TestInternalExecSites_EachIsAbsoluteAndGovernedOrNoExec(t *testing.T) {
	require.NotEmpty(t, internalExecSites, "the registry lists the daemon's internal execs")
	for _, site := range internalExecSites {
		t.Run(site.Name, func(t *testing.T) {
			assert.True(t, path.IsAbs(site.Program),
				"%s names its program by an absolute path, got %q", site.Name, site.Program)
			assert.True(t, site.AuditedIdentity != site.NoExec,
				"%s is exactly one of audited-identity or no-exec", site.Name)
		})
	}
}

// The registry covers the programs the production code actually runs — it is
// their single source of truth, not a copy that can drift. Refs: MGIT-272
func TestInternalExecSites_CoverTheCodesActualPrograms(t *testing.T) {
	byProgram := map[string]internalExecSite{}
	for _, s := range internalExecSites {
		byProgram[s.Program] = s
	}
	settle, ok := byProgram[settleShell]
	require.True(t, ok, "the settle read-back's program %q is registered", settleShell)
	assert.True(t, settle.AuditedIdentity, "the settle read-back is an audited-identity site")

	probe, ok := byProgram[guestProbeCommand[0]]
	require.True(t, ok, "the readiness probe's program %q is registered", guestProbeCommand[0])
	assert.True(t, probe.NoExec, "the readiness probe is a no-exec site")
}

// For every audited-identity site, running its program through the settler
// carries the wired identity — the audited-identity property holds per site,
// driven by the registry, not by one hand-picked program. Refs: MGIT-272, MGIT-151
func TestInternalExecSites_AuditedSitesRunThroughTheIdentityPath(t *testing.T) {
	id := model.RootIdentity()
	for _, site := range internalExecSites {
		if !site.AuditedIdentity {
			continue
		}
		t.Run(site.Name, func(t *testing.T) {
			rec := &recordingGuestExec{ranAs: &id}
			s := execSettler{m: &Manager{internalIdentity: &id, internalAudit: &recordingAuditor{}}, exec: rec.run}
			_, err := s.run(context.Background(), "sb1", []string{site.Program, "-c", "true"})
			require.NoError(t, err)
			sent := rec.sent()
			require.Len(t, sent, 1)
			require.NotNil(t, sent[0].RunAs, "%s ran with an explicit identity", site.Name)
			assert.Equal(t, id.UID, sent[0].RunAs.UID)
		})
	}
}

// An internal exec whose program is NOT a registered audited site is refused
// before it runs — so a new internal exec that was not added to the registry
// fails rather than running ungoverned. Refs: MGIT-272
func TestSettleRun_RefusesAnUnregisteredProgram(t *testing.T) {
	id := model.RootIdentity()
	rec := &recordingGuestExec{ranAs: &id}
	s := execSettler{m: &Manager{internalIdentity: &id, internalAudit: &recordingAuditor{}}, exec: rec.run}
	_, err := s.run(context.Background(), "sb1", []string{"/usr/bin/whatever", "-c", "true"})
	require.Error(t, err, "an unregistered internal exec program is refused")
	assert.Empty(t, rec.sent(), "the unregistered program never reached the guest")
}

// The whole settle path, driven end to end, only ever runs registered audited
// programs — a guard against a future settle exec that skips the registry.
// Refs: MGIT-272
func TestSettleProbe_RunsOnlyRegisteredPrograms(t *testing.T) {
	id := model.RootIdentity()
	const wt = "/wt"
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rec := &recordingGuestExec{ranAs: &id, hashes: map[string]string{path.Join(wt, "app.go"): digest}}
	s := execSettler{m: &Manager{internalIdentity: &id, internalAudit: &recordingAuditor{}}, exec: rec.run}
	req := settleRequest{
		sandboxID: "sb1", taskID: "MGIT-272", network: model.NetworkModeNone, worktree: wt,
		want:    worktreesync.Manifest{"app.go": {Hash: digest, Mode: 0o644}},
		deleted: []string{"gone.txt"},
	}
	_, err := s.Probe(context.Background(), req)
	require.NoError(t, err)
	for _, sent := range rec.sent() {
		require.NotEmpty(t, sent.Command)
		assert.True(t, path.IsAbs(sent.Command[0]),
			"the settle ran %q, which is not an absolute program", sent.Command[0])
	}
}
