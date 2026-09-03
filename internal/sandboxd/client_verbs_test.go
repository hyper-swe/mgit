package sandboxd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// clientFor starts a daemon with the given collaborators wired and returns a
// client speaking to it over a real socket.
//
// The five verbs below were at ZERO client-side coverage. Their DISPATCH
// halves are well tested (grant_dispatch_test, policy_dispatch_test,
// export_dispatch_test) — what nothing exercised is that the client encodes
// the request the dispatch expects and reads back the field the dispatch
// filled. A client that sent the wrong Kind, or read `resp.Policy` where the
// daemon wrote `resp.Granted`, would pass every existing test in this package.
func clientFor(t *testing.T, wire func(*Config)) *Client {
	t.Helper()
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{})
	wire(&cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()
	t.Cleanup(func() { cancel(); <-done })
	return NewClient(cfg.SocketPath, time.Now)
}

// Grants and Grant complete a round trip and carry back what the operator has
// to act on: the observed destination behind each pending request, and the key
// that names it. Refs: FR-17.12, SEC-05
func TestClient_GrantsAndGrant_RoundTrip(t *testing.T) {
	pending := model.CapabilityRequest{
		Capability: model.CapabilityEgress, ObservedDestIP: "203.0.113.7", ObservedDestPort: 443,
	}
	gc := &fakeGrantCoordinator{
		pending: []model.CapabilityRequest{pending},
		grant: &model.CapabilityGrant{
			Capability: model.CapabilityEgress, ObservedDestIP: "203.0.113.7", ObservedDestPort: 443,
		},
	}
	client := clientFor(t, func(c *Config) { c.Grants = gc })
	ctx := context.Background()

	got, err := client.Grants(ctx, "MGIT-1")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "203.0.113.7", got[0].DestIP,
		"an operator approves a DESTINATION; a request without one cannot be judged")
	assert.Equal(t, 443, got[0].DestPort)
	assert.Equal(t, pending.Key(), got[0].Key,
		"the key must be the one Approve expects, or the round trip cannot be completed")

	approved, err := client.Grant(ctx, "MGIT-1", got[0].Key)

	require.NoError(t, err)
	require.NotNil(t, approved)
	assert.Equal(t, "203.0.113.7", approved.DestIP)
	assert.Equal(t, got[0].Key, gc.approveKey,
		"the key the client listed must be the key the coordinator was handed")
}

// The two policy verbs must not be confusable. Set REPLACES what is enforced
// and Show only reports it, so a client that sent the wrong Kind would mutate
// a running sandbox's containment on a read. Both are asserted in one test
// against one daemon, which is where a swap would show. Refs: MGIT-72, SEC-04
func TestClient_PolicySetAndShow_AreDistinctVerbs(t *testing.T) {
	pc := &countingPolicy{
		change: &model.EgressPolicyChange{Entries: []string{"example.com"}, Killed: 3},
		state:  &model.EgressPolicyState{Entries: []string{"example.com"}},
	}
	client := clientFor(t, func(c *Config) { c.Policy = pc })
	ctx := context.Background()

	set, err := client.SetEgressPolicy(ctx, "MGIT-1", []string{"example.com"}, false)

	require.NoError(t, err)
	require.NotNil(t, set)
	assert.Equal(t, []string{"example.com"}, pc.gotEntry)
	assert.False(t, pc.gotDrain, "the default terminates established flows (ADR-012)")
	assert.Equal(t, 3, set.Killed,
		"the reply states what was ENFORCED, not what was asked for")
	assert.Equal(t, 1, pc.sets)

	show, err := client.EgressPolicy(ctx, "MGIT-1")

	require.NoError(t, err)
	require.NotNil(t, show)
	assert.Equal(t, 1, pc.shows, "the read reached Show")
	// COUNTED, not inferred from the arguments. Set(nil) and "Set was never
	// called" leave identical argument state, so an assertion on gotEntry
	// cannot tell them apart — a client sending KindPolicySet for a read
	// would pass it. That version of this test existed and was deleted.
	assert.Equal(t, 1, pc.sets,
		"a read must never reach Set: showing a policy cannot change one")
}

// countingPolicy is fakePolicyCoordinator plus call counts, so "Set was not
// called" is distinguishable from "Set was called with nothing".
type countingPolicy struct {
	sets, shows int
	gotInfo     model.SandboxInfo
	gotEntry    []string
	gotDrain    bool
	change      *model.EgressPolicyChange
	state       *model.EgressPolicyState
	err         error
}

func (f *countingPolicy) Set(
	_ context.Context, info model.SandboxInfo, entries []string, drain bool,
) (*model.EgressPolicyChange, error) {
	f.sets++
	f.gotInfo, f.gotEntry, f.gotDrain = info, entries, drain
	if f.err != nil {
		return nil, f.err
	}
	return f.change, nil
}

func (f *countingPolicy) Show(
	_ context.Context, info model.SandboxInfo,
) (*model.EgressPolicyState, error) {
	f.shows++
	f.gotInfo = info
	if f.err != nil {
		return nil, f.err
	}
	return f.state, nil
}

// --drain is a containment decision, not a formatting one: a draining
// connection is exactly the exfiltration channel the caller just revoked, so
// whether it survives has to cross the wire intact. Refs: MGIT-72, ADR-012
func TestClient_SetEgressPolicy_DrainCrossesTheWire(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminate", true: "drain"}[drain], func(t *testing.T) {
			pc := &countingPolicy{change: &model.EgressPolicyChange{}}
			client := clientFor(t, func(c *Config) { c.Policy = pc })

			_, err := client.SetEgressPolicy(context.Background(), "MGIT-1", nil, drain)

			require.NoError(t, err)
			assert.Equal(t, drain, pc.gotDrain)
		})
	}
}

// An EMPTY entry list is a full revoke, not "no change". It must survive the
// round trip as an empty list rather than being dropped by omitempty into a
// request the daemon reads as a no-op. Refs: MGIT-72, SEC-04
func TestClient_SetEgressPolicy_AFullRevokeIsNotLostOnTheWire(t *testing.T) {
	pc := &countingPolicy{change: &model.EgressPolicyChange{}}
	client := clientFor(t, func(c *Config) { c.Policy = pc })

	_, err := client.SetEgressPolicy(context.Background(), "MGIT-1", []string{}, false)

	require.NoError(t, err)
	// Asserted as REACHED-AND-EMPTY, not as a non-nil slice. `omitempty` does
	// drop the empty list on the wire, and that is harmless by design: the
	// receiver distinguishes nil from empty in exactly one place —
	// service.nonNil — which normalizes both to `[]` so the audit record reads
	// "everything was revoked" instead of `null`. Asserting non-nil here would
	// pin an encoding detail the product deliberately does not depend on.
	require.Equal(t, 1, pc.sets, "the revoke must reach the enforcer, not be read as a no-op")
	assert.Empty(t, pc.gotEntry, "and it must arrive as a revoke, not as some other policy")
}

// ExportArtifact carries both HOST-NAMED paths out and the crossing record
// back. Both paths are host-supplied by design (MGIT-73) — the guest never
// participates — so a client that dropped either would let the daemon choose,
// which is the thing the design forbids. Refs: MGIT-73, ADR-011
func TestClient_ExportArtifact_RoundTrip(t *testing.T) {
	ex := &fakeExporter{}
	client := clientFor(t, func(c *Config) { c.Exporter = ex })

	got, err := client.ExportArtifact(context.Background(), "MGIT-1",
		model.ArtifactExportRequest{GuestPath: "dist/app", HostPath: "/host/out/app"})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "MGIT-1", ex.taskID)
	assert.Equal(t, "dist/app", ex.req.GuestPath, "the host names the guest path")
	assert.Equal(t, "/host/out/app", ex.req.HostPath, "and the host names the destination")
	assert.Equal(t, 2, got.Files, "the crossing record comes back")
	assert.NotEmpty(t, got.ManifestPath, "including where the manifest was written")
}

// Every optional verb must REFUSE when its collaborator is not wired, and the
// refusal must be one an operator can act on.
//
// Those are two claims and they hold differently, so they are asserted
// separately. Refusing at all holds everywhere. Being actionable holds only
// for the policy verbs, which MGIT-104 fixed; the other four still answer with
// a hex opcode, filed as MGIT-171 and skipped here.
//
// The verbs are enumerated from controlproto's own Kind constants rather than
// from a list this test maintains, so a sixth optional verb inherits the
// requirement instead of waiting for someone to remember it.
// Refs: MGIT-171, MGIT-104, MGIT-111
func TestClient_OptionalVerbs_ReportThemselvesUnservedWhenUnwired(t *testing.T) {
	client := clientFor(t, func(*Config) {}) // nothing optional wired
	ctx := context.Background()

	calls := map[byte]func() error{
		controlproto.KindGrants: func() error {
			_, err := client.Grants(ctx, "MGIT-1")
			return err
		},
		controlproto.KindGrant: func() error {
			_, err := client.Grant(ctx, "MGIT-1", "k")
			return err
		},
		controlproto.KindPolicySet: func() error {
			_, err := client.SetEgressPolicy(ctx, "MGIT-1", nil, false)
			return err
		},
		controlproto.KindPolicyShow: func() error {
			_, err := client.EgressPolicy(ctx, "MGIT-1")
			return err
		},
		controlproto.KindExport: func() error {
			_, err := client.ExportArtifact(ctx, "MGIT-1",
				model.ArtifactExportRequest{GuestPath: "dist/app", HostPath: "/host/out/app"})
			return err
		},
		controlproto.KindLand: func() error {
			_, err := client.Land(ctx, "MGIT-1")
			return err
		},
	}
	for kind, call := range calls {
		t.Run(kindNames[kind], func(t *testing.T) {
			err := call()

			require.Error(t, err, "an unwired verb must refuse, not succeed emptily")

			if issue, blocked := unactionableRefusal[kind]; blocked {
				t.Skip(issue)
			}
			// An operator has to be able to tell "this build cannot serve
			// that" from "this call failed" — the two need opposite
			// responses. A hex opcode says neither.
			assert.NotRegexp(t, `kind 0x[0-9a-f]+`, err.Error(),
				"a refusal must not hand the reader a wire opcode to decode")
			assert.Contains(t, err.Error(), "this daemon",
				"the refusal must say it is this daemon that cannot serve the verb")
			assert.Contains(t, err.Error(), "nothing was changed",
				"and that nothing happened, so the caller knows there is nothing to undo")
		})
	}
}

// unactionableRefusal marks the verbs MGIT-104's fix has not reached. Each
// entry is a skip, never a narrowed assertion: the case above is written whole
// and turns red the moment the entry is deleted. Refs: MGIT-171
var unactionableRefusal = map[byte]string{
	controlproto.KindGrants: "MGIT-171: answers with a hex opcode",
	controlproto.KindGrant:  "MGIT-171: answers with a hex opcode",
	controlproto.KindExport: "MGIT-171: answers with a hex opcode",
	controlproto.KindLand:   "MGIT-171: answers with a hex opcode — on the audit-critical verb",
}

// The policy verbs are the ones MGIT-104's fix DID reach, and they are the
// worked example the other four should follow: they name the backend, state
// the enforcement fact, point at what to use instead, and say nothing was
// changed. Pinned so the fix cannot regress while its siblings wait.
// Refs: MGIT-111, MGIT-104
func TestClient_PolicyRefusal_IsTheWorkedExampleTheOthersOwe(t *testing.T) {
	client := clientFor(t, func(*Config) {})

	_, err := client.EgressPolicy(context.Background(), "MGIT-1")

	require.Error(t, err)
	msg := err.Error()
	assert.NotRegexp(t, `kind 0x[0-9a-f]+`, msg, "no wire opcode reaches the operator")
	for _, want := range []string{
		"no live egress allowlist", // the enforcement fact
		"--network none",           // what to do instead
		"backend that enforces",    // and the other way out
		"nothing was changed",      // and that this was a refusal, not damage
	} {
		assert.Contains(t, msg, want,
			"the policy refusal must keep naming what an operator can act on")
	}
}

// kindNames labels the optional verbs for subtest output. The KINDS come from
// controlproto's own constants above; only the human label lives here.
var kindNames = map[byte]string{
	controlproto.KindGrants:     "grants",
	controlproto.KindGrant:      "grant",
	controlproto.KindPolicySet:  "policy_set",
	controlproto.KindPolicyShow: "policy_show",
	controlproto.KindExport:     "export",
	controlproto.KindLand:       "land",
}

// A verb whose task cannot be resolved fails with the resolution error, not
// with a grant/policy error. Conflating them would send an operator to the
// egress rules over a task ID typo. Refs: SEC-05
func TestClient_TaskThatCannotBeResolved_FailsWithTheResolutionError(t *testing.T) {
	skipUnsupportedHostIPC(t)
	cfg, _ := dispatchConfig(t, &fakeDispatcher{opErr: model.ErrSandboxNotFound})
	cfg.Grants = &fakeGrantCoordinator{}
	cfg.Policy = &fakePolicyCoordinator{}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()
	defer func() { cancel(); <-done }()
	client := NewClient(cfg.SocketPath, time.Now)

	_, gErr := client.Grants(context.Background(), "NOPE")
	_, pErr := client.EgressPolicy(context.Background(), "NOPE")

	for name, err := range map[string]error{"grants": gErr, "policy show": pErr} {
		require.Error(t, err, "%s must fail", name)
		assert.Contains(t, err.Error(), model.ErrSandboxNotFound.Error(),
			"%s must report the task that could not be resolved, not a downstream error", name)
	}
}

// A coordinator failure reaches the caller intact. A verb that swallowed it
// and returned a nil result would read as "approved, nothing to report".
func TestClient_ACoordinatorFailure_ReachesTheCaller(t *testing.T) {
	boom := errors.New("nftables set is not present")
	client := clientFor(t, func(c *Config) {
		c.Grants = &fakeGrantCoordinator{approveErr: boom}
		c.Policy = &fakePolicyCoordinator{err: boom}
	})
	ctx := context.Background()

	granted, gErr := client.Grant(ctx, "MGIT-1", "k")
	require.Error(t, gErr)
	assert.Nil(t, granted, "a failed approval must not look like a silent one")
	assert.Contains(t, gErr.Error(), boom.Error())

	_, pErr := client.SetEgressPolicy(ctx, "MGIT-1", []string{"x"}, false)
	require.Error(t, pErr)
	assert.Contains(t, pErr.Error(), boom.Error())
}

// stallingWatcher blocks for d and CANNOT be canceled.
//
// That is not laziness: the production watcher's work is
// gitstore.WorkingTreeFingerprint, a filesystem walk that takes no context.
// A cancellable stub is the difference between seeing MGIT-170 and not — the
// first version of this repro used one and measured 507us against the same
// broken code. Refs: MGIT-170, MGIT-110
type stallingWatcher struct{ d time.Duration }

func (w stallingWatcher) Observe(context.Context) error {
	time.Sleep(w.d)
	return nil
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-170.
//
// The snapshot pass runs INLINE in the daemon's select loop, so a client's
// greeting waits for the whole in-flight pass. Everything else the daemon does
// is deliberately kept off that path — connections get a per-connection
// goroutine precisely so "a slow or hung client must never block idle checks
// or shutdown". The pass is the one piece of work that does block them, and
// the one whose duration scales with the user's repository.
//
// Measured: dial 167us, first byte 1.40s. Refs: MGIT-170, MGIT-110
func TestDaemon_ASnapshotPassInFlight_DoesNotStallARequest(t *testing.T) {
	t.Skip("MGIT-170: the snapshot pass runs inline in the select loop and stalls the request path")

	skipUnsupportedHostIPC(t)
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = stallingWatcher{d: 2 * time.Second}
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()
	time.Sleep(150 * time.Millisecond) // let a pass get under way

	start := time.Now()
	conn, err := net.Dial("unix", cfg.SocketPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, len(greeting))
	_, err = conn.Read(buf)
	require.NoError(t, err)

	assert.Less(t, time.Since(start), 500*time.Millisecond,
		"housekeeping must not sit in front of the request path")
	cancel()
	<-done
}

// EXPECTED TO FAIL — SKIPPED, NAMING MGIT-170.
//
// The serious half. Drain is what writes the terminal `destroyed` event and
// reaps the VMs; a daemon SIGKILLed before reaching it leaves them
// unsupervised and the next daemon stamps `killed / unsupervised` — the exact
// record MGIT-107 was closed to stop manufacturing.
//
// Measured 7.9s against a 2s pass, which is MORE than one pass: once a pass
// outlasts the tick interval, snapTicker.C is always ready when the select is
// re-entered and Go chooses uniformly at random among ready cases, so each
// iteration is a coin flip between shutting down and starting another pass.
// The delay has no bound, only a geometric tail. Refs: MGIT-170, MGIT-107, MGIT-110
func TestDaemon_ASnapshotPassInFlight_DoesNotDelayShutdown(t *testing.T) {
	t.Skip("MGIT-170: shutdown waits for one or more full snapshot passes")

	skipUnsupportedHostIPC(t)
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = stallingWatcher{d: 2 * time.Second}
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	cancel()
	require.NoError(t, <-done)

	assert.Less(t, time.Since(start), time.Second,
		"a shutdown that waits on housekeeping can miss a supervisor's SIGKILL window, "+
			"and a daemon killed before its drain leaves the orphaned VMs and the "+
			"crash record MGIT-107 exists to prevent")
}

// The regression control for MGIT-170's eventual fix: whatever moves the pass
// off the select loop must not stop it happening. If a fix made passes fire
// and be dropped, this catches it. Refs: MGIT-170, MGIT-110
func TestDaemon_TheWatcherIsStillDrivenWhilePassesAreSlow(t *testing.T) {
	watcher := &countingWatcher{}
	cfg, _ := testConfig(t, newFakeManager("01JXSB1"))
	cfg.IdleGrace = time.Hour
	cfg.Watcher = watcher
	cfg.SnapshotInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(ctx, t, cfg)
	_ = waitForSocket(t, cfg.SocketPath).Close()

	require.Eventually(t, func() bool { return watcher.count() >= 3 },
		2*time.Second, 10*time.Millisecond,
		"passes must keep happening; a fix that silences them is not a fix")

	cancel()
	require.NoError(t, <-done)
}
