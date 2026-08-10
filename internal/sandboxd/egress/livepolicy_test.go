package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllowlist_SetRules_TakesEffectOnTheNextDecision is the core of MGIT-72:
// a running sandbox's policy can be narrowed, and the very next authorization
// sees the new rules. Refs: MGIT-72, SEC-04
func TestAllowlist_SetRules_TakesEffectOnTheNextDecision(t *testing.T) {
	al, err := Compile([]string{"registry.example:443"})
	require.NoError(t, err)
	require.True(t, al.AllowsName("registry.example", 443), "allowed before the revoke")

	require.NoError(t, al.SetRules(nil))

	assert.False(t, al.AllowsName("registry.example", 443), "refused after the revoke")
}

// TestAllowlist_SetRules_CanWidenToo verifies the same primitive grants as
// well as revokes — the grant-then-revoke sequence HyperSwe runs.
func TestAllowlist_SetRules_CanWidenToo(t *testing.T) {
	al, err := Compile(nil)
	require.NoError(t, err)
	require.False(t, al.AllowsName("registry.example", 443))

	require.NoError(t, al.SetRules([]string{"registry.example:443"}))

	assert.True(t, al.AllowsName("registry.example", 443))
}

// TestAllowlist_SetRules_DropsLiveGrants verifies a policy change invalidates
// prior ad-hoc widenings. A grant that outlived the policy it was granted
// under would be a hole nobody could see in the new policy. Refs: SEC-05
func TestAllowlist_SetRules_DropsLiveGrants(t *testing.T) {
	al, err := Compile([]string{"registry.example:443"})
	require.NoError(t, err)
	granted := netip.MustParseAddr("140.82.112.3")
	require.NoError(t, al.GrantIP(granted, 443))
	require.True(t, al.AllowsIP(granted, 443))

	require.NoError(t, al.SetRules([]string{"registry.example:443"}))

	assert.False(t, al.AllowsIP(granted, 443),
		"a live grant must not survive the policy change it was granted under")
}

// TestAllowlist_SetRules_RejectsAMalformedPolicyWithoutApplyingIt is the
// atomicity guarantee: a policy that does not compile leaves the running one
// untouched, so a flow is never authorized against a half-applied ruleset.
// Refs: MGIT-72
func TestAllowlist_SetRules_RejectsAMalformedPolicyWithoutApplyingIt(t *testing.T) {
	al, err := Compile([]string{"registry.example:443"})
	require.NoError(t, err)

	err = al.SetRules([]string{"registry.example:443", "not a valid entry !!"})

	require.Error(t, err)
	assert.True(t, al.AllowsName("registry.example", 443),
		"the previous policy must still be in force after a rejected change")
}

// TestAllowlist_SetRules_IsRaceFreeUnderConcurrentDecisions runs decisions
// against a policy being swapped underneath them.
//
// NOTE ON WHAT ATOMICITY MEANS HERE, because the obvious test is wrong: two
// consecutive authorizations are two SEPARATE decisions, and the policy may
// legitimately change between them — asserting they agree would fail by
// design. The property is that ONE decision never sees a half-applied
// ruleset, which holds because the replacement is compiled before the lock and
// swapped in a single assignment. This exercises that path concurrently so the
// race detector can speak to it, and asserts every individual answer is one of
// the two policies' valid answers. Refs: MGIT-72
func TestAllowlist_SetRules_IsRaceFreeUnderConcurrentDecisions(t *testing.T) {
	al, err := Compile([]string{"a.example:443"})
	require.NoError(t, err)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Whatever the answer, it must be a decision the ALLOWLIST
				// could legitimately produce — never a panic or a torn read.
				_ = al.AllowsName("a.example", 443)
				_ = al.AllowsIP(netip.MustParseAddr("140.82.112.3"), 443)
			}
		}()
	}
	for i := 0; i < 300; i++ {
		require.NoError(t, al.SetRules([]string{"a.example:443"}))
		require.NoError(t, al.SetRules(nil))
	}
	close(stop)
	wg.Wait()

	// The final state is the last policy applied, in full.
	assert.False(t, al.AllowsName("a.example", 443))
	require.NoError(t, al.SetRules([]string{"a.example:443"}))
	assert.True(t, al.AllowsName("a.example", 443))
}

// pipePair returns two connected TCP conns for flow-tracking assertions.
func pipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	done := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			done <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	server := <-done
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

// TestFlowRegistry_CloseAll_KillsEstablishedFlows is the established-flow
// decision made real: revoke KILLS in flight connections by default.
//
// A caller who revokes registry egress and then runs untrusted code expects
// the grant gone; a draining connection is precisely the exfiltration channel
// they just revoked, and against a hostile guest a long-lived one can drain
// arbitrarily long. Refs: MGIT-72, ADR-012, SEC-04
func TestFlowRegistry_CloseAll_KillsEstablishedFlows(t *testing.T) {
	reg := NewFlowRegistry()
	guest, peer := pipePair(t)
	release := reg.Track(guest, peer)
	defer release()

	killed := reg.CloseAll()

	assert.Equal(t, 1, killed, "the established flow is counted as killed")
	_ = guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := guest.Read(make([]byte, 1))
	require.Error(t, err, "a killed flow must not stay readable")
}

// TestFlowRegistry_Release_StopsTracking verifies a flow that ended on its own
// is not held forever — the registry must not become a leak of dead conns.
func TestFlowRegistry_Release_StopsTracking(t *testing.T) {
	reg := NewFlowRegistry()
	guest, peer := pipePair(t)

	release := reg.Track(guest, peer)
	assert.Equal(t, 1, reg.Len())
	release()

	assert.Equal(t, 0, reg.Len())
	assert.Equal(t, 0, reg.CloseAll(), "nothing is left to kill")
}

// TestFlowRegistry_Drain_LeavesEstablishedFlowsAlone verifies the opt-in
// weaker behavior: with drain, in-flight connections finish.
func TestFlowRegistry_Drain_LeavesEstablishedFlowsAlone(t *testing.T) {
	reg := NewFlowRegistry()
	guest, peer := pipePair(t)
	defer reg.Track(guest, peer)()

	// Draining is simply "do not CloseAll" — the registry is untouched.
	assert.Equal(t, 1, reg.Len())

	go func() { _, _ = peer.Write([]byte("X")) }()
	_ = guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := guest.Read(buf)

	require.NoError(t, err, "a drained flow keeps working")
	assert.Equal(t, 1, n)
}

// TestFlowRegistry_IsSafeUnderConcurrency guards the registry against the
// obvious races: tracking and killing happen from different goroutines.
func TestFlowRegistry_IsSafeUnderConcurrency(t *testing.T) {
	reg := NewFlowRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			guest, peer := net.Pipe()
			release := reg.Track(guest, peer)
			release()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			reg.CloseAll()
		}
	}()
	wg.Wait()
	assert.Equal(t, 0, reg.Len())
}

// TestSpliceTracked_RegistersAndReleases verifies the data path registers its
// flows, so a revoke can actually reach them — a registry nothing registers
// with would kill zero connections and look like it worked.
func TestSpliceTracked_RegistersAndReleases(t *testing.T) {
	reg := NewFlowRegistry()
	guestA, guestB := pipePair(t)
	peerA, peerB := pipePair(t)

	done := make(chan struct{})
	go func() { SpliceTracked(reg, guestB, peerA); close(done) }()

	// While the splice runs the flow is tracked.
	require.Eventually(t, func() bool { return reg.Len() == 1 }, 2*time.Second, 10*time.Millisecond)

	// Killing it ends the splice and clears the registry.
	assert.Equal(t, 1, reg.CloseAll())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the splice did not end when its flow was killed")
	}
	assert.Equal(t, 0, reg.Len())
	_ = guestA.Close()
	_ = peerB.Close()
	_, _ = io.Discard.Write(nil)
}

// TestFlowRegistry_NilIsUsable verifies a nil registry is a no-op, so a data
// path that has none (the libkrun child gateway today) needs no branch.
func TestFlowRegistry_NilIsUsable(t *testing.T) {
	var reg *FlowRegistry
	guest, peer := net.Pipe()

	release := reg.Track(guest, peer)

	assert.NotNil(t, release)
	release()
	assert.Equal(t, 0, reg.CloseAll())
	assert.Equal(t, 0, reg.Len())
	_ = guest.Close()
	_ = peer.Close()
	_ = context.Background()
}

// TestRunner_SetPolicy_AllowThenRevoke_ThroughTheRealProxy is the MGIT-72
// round trip HyperSwe actually runs, with BOTH halves asserted: the flow is
// allowed and carries real bytes, then after the revoke the same destination
// is refused — and refused by POLICY, not because the stack died.
// Refs: MGIT-72, SEC-04
func TestRunner_SetPolicy_AllowThenRevoke_ThroughTheRealProxy(t *testing.T) {
	rec := &dialRecorder{}
	r := testRunner(t, rec)
	gw := netip.MustParseAddr("127.0.0.1")
	eps, err := r.Start(context.Background(), Binding{
		SandboxID: "sbx-72", TaskID: "MGIT-72", GatewayIP: gw,
		Policy: allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("sbx-72") }()

	// ALLOW half: the destination is admitted while the policy names it.
	allowed := connectThroughProxy(t, eps.ProxyAddr, "registry.npmjs.org", 443)
	require.True(t, allowed, "the flow must be allowed BEFORE the revoke")

	// Revoke it on the RUNNING sandbox.
	change, err := r.SetPolicy("sbx-72", nil, false)
	require.NoError(t, err)
	assert.Zero(t, change.RuleCount, "the new policy names nothing")

	// DENY half: the same destination is now refused.
	stillAllowed := connectThroughProxy(t, eps.ProxyAddr, "registry.npmjs.org", 443)
	assert.False(t, stillAllowed, "the same destination must be refused AFTER the revoke")

	// And the refusal is a POLICY decision, not a dead stack: the proxy still
	// answers, and re-granting makes the very next flow succeed again.
	_, err = r.SetPolicy("sbx-72", []string{"registry.npmjs.org"}, false)
	require.NoError(t, err)
	assert.True(t, connectThroughProxy(t, eps.ProxyAddr, "registry.npmjs.org", 443),
		"re-granting must work, proving the earlier denial was policy and not breakage")
}

// awaitTrackedFlows blocks until a sandbox has exactly want flows registered.
//
// WHY THIS IS NECESSARY. A client that has read the CONNECT reply has NOT
// observed the flow being tracked: the proxy writes the reply (proxy.go) and
// only then calls SpliceTracked, so between those two statements the registry
// is legitimately empty. A revoke issued in that window kills nothing and the
// assertion fails — which is exactly how this test flaked on CI while passing
// hundreds of local runs.
//
// The reply is the wrong signal, so the fix is to synchronize on the real
// condition rather than to sleep. A sleep would only narrow the window; this
// closes it, and it fails with a diagnosis rather than a bare count mismatch.
// Refs: MGIT-72
func trackedFlows(t *testing.T, r *Runner, sandboxID string) int {
	t.Helper()
	r.mu.Lock()
	ae, ok := r.active[sandboxID]
	r.mu.Unlock()
	if !ok {
		t.Fatalf("sandbox %q is not running", sandboxID)
	}
	return ae.sup.Flows().Len()
}

func awaitTrackedFlows(t *testing.T, r *Runner, sandboxID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		r.mu.Lock()
		ae, ok := r.active[sandboxID]
		r.mu.Unlock()
		if ok {
			if got = ae.sup.Flows().Len(); got == want {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("sandbox %q tracked %d flows, want %d, after 5s — the proxy "+
		"registers a flow only after replying, so a revoke asserted on the "+
		"reply alone would race this", sandboxID, got, want)
}

// TestRunner_SetPolicy_KillsEstablishedFlows verifies the documented default:
// revoke terminates in-flight connections. Refs: MGIT-72, ADR-012
func TestRunner_SetPolicy_KillsEstablishedFlows(t *testing.T) {
	rec := &dialRecorder{}
	r := testRunner(t, rec)
	eps, err := r.Start(context.Background(), Binding{
		SandboxID: "sbx-kill", TaskID: "MGIT-72",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy:    allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("sbx-kill") }()

	conn, err := net.Dial("tcp", eps.ProxyAddr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, EncodeConnectRequest(conn, ConnectRequest{
		Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(conn)
	require.NoError(t, err)
	require.True(t, allow, "the flow is established before the revoke")
	// The reply does not mean the flow is tracked yet — wait for the state the
	// revoke actually acts on, or this races (see awaitTrackedFlows).
	awaitTrackedFlows(t, r, "sbx-kill", 1)

	change, err := r.SetPolicy("sbx-kill", nil, false)

	require.NoError(t, err)
	assert.Positive(t, change.Killed, "the established flow must be terminated by a revoke")
	assert.False(t, change.Drained)
}

// TestRunner_SetPolicy_Drain_LeavesEstablishedFlowsAlone verifies the opt-in
// weaker behavior is honored and reported as such.
func TestRunner_SetPolicy_Drain_LeavesEstablishedFlowsAlone(t *testing.T) {
	rec := &dialRecorder{}
	r := testRunner(t, rec)
	eps, err := r.Start(context.Background(), Binding{
		SandboxID: "sbx-drain", TaskID: "MGIT-72",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy:    allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("sbx-drain") }()
	// HOLD the connection open across the revoke. This test used to use
	// connectThroughProxy, which CLOSES the connection before returning — so
	// there was never an established flow and Killed==0 passed vacuously,
	// exactly as it would against a registry that tracked nothing at all. The
	// awaitTrackedFlows below is what makes this assertion mean anything.
	// Refs: MGIT-72
	conn, err := net.Dial("tcp", eps.ProxyAddr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, EncodeConnectRequest(conn, ConnectRequest{
		Protocol: "tcp", Host: "registry.npmjs.org", Port: 443}))
	allow, _, err := DecodeConnectReply(conn)
	require.NoError(t, err)
	require.True(t, allow, "the flow is established before the drain")
	awaitTrackedFlows(t, r, "sbx-drain", 1)

	change, err := r.SetPolicy("sbx-drain", nil, true)

	require.NoError(t, err)
	assert.Zero(t, change.Killed, "draining kills nothing")
	assert.True(t, change.Drained, "the weaker behavior is reported, not silent")
	// The flow is not merely uncounted — it is still REGISTERED, i.e. alive.
	// Without this, "killed 0" and "killed everything and forgot" look alike.
	assert.Equal(t, 1, trackedFlows(t, r, "sbx-drain"),
		"a drained flow must survive the revoke, not merely go uncounted")
}

// TestRunner_SetPolicy_UnknownSandbox_FailsClosed verifies "revoke succeeded"
// is never reported for a sandbox that was not enforcing — the most dangerous
// possible lie from this verb.
func TestRunner_SetPolicy_UnknownSandbox_FailsClosed(t *testing.T) {
	r := testRunner(t, &dialRecorder{})

	_, err := r.SetPolicy("no-such-sandbox", nil, false)

	require.Error(t, err)
}

// TestRunner_SetPolicy_MalformedPolicy_LeavesTheRunningOneInForce verifies
// atomicity at the runner: a policy that does not compile changes nothing.
func TestRunner_SetPolicy_MalformedPolicy_LeavesTheRunningOneInForce(t *testing.T) {
	rec := &dialRecorder{}
	r := testRunner(t, rec)
	eps, err := r.Start(context.Background(), Binding{
		SandboxID: "sbx-bad", TaskID: "MGIT-72",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy:    allowlistPolicy("registry.npmjs.org"),
	})
	require.NoError(t, err)
	defer func() { _ = r.Stop("sbx-bad") }()

	_, err = r.SetPolicy("sbx-bad", []string{"not a valid entry !!"}, false)

	require.Error(t, err)
	assert.True(t, connectThroughProxy(t, eps.ProxyAddr, "registry.npmjs.org", 443),
		"the previous policy must still be enforced after a rejected change")
}

// connectThroughProxy drives one CONNECT through the running proxy and
// reports whether it was admitted.
func connectThroughProxy(t *testing.T, proxyAddr, host string, port int) bool {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, EncodeConnectRequest(conn, ConnectRequest{Protocol: "tcp", Host: host, Port: port}))
	allow, _, err := DecodeConnectReply(conn)
	require.NoError(t, err)
	return allow
}

// TestAllowlist_Rules_ReportsTheEntriesInForce verifies the compiled policy
// can state what it is enforcing, not merely how many rules it holds.
//
// This is what lets a caller OBSERVE a live mutation. Without it, the only
// readable policy is the launch-time one, which after a revoke is a lie — and
// a revoke a caller cannot confirm is a revoke they have to take on faith.
// Refs: MGIT-72
func TestAllowlist_Rules_ReportsTheEntriesInForce(t *testing.T) {
	al, err := Compile([]string{"registry.example:443", "10.1.0.0/16"})
	require.NoError(t, err)
	assert.Equal(t, []string{"registry.example:443", "10.1.0.0/16"}, al.Rules())

	require.NoError(t, al.SetRules([]string{"proxy.example:8080"}))
	assert.Equal(t, []string{"proxy.example:8080"}, al.Rules(),
		"Rules must follow a live mutation, not report the launch policy")

	require.NoError(t, al.SetRules(nil))
	assert.Empty(t, al.Rules(), "a full revoke enforces nothing and must say so")
}

// TestAllowlist_Rules_IsACopy verifies a caller cannot mutate the running
// policy by writing into the slice it was handed.
func TestAllowlist_Rules_IsACopy(t *testing.T) {
	al, err := Compile([]string{"registry.example:443"})
	require.NoError(t, err)

	got := al.Rules()
	got[0] = "evil.example:443"

	assert.Equal(t, []string{"registry.example:443"}, al.Rules())
}

// TestRunner_Policy_ReportsTheLivePolicy verifies the host-side runner can
// report what a RUNNING sandbox is enforcing right now, including after a
// mutation. Refs: MGIT-72
func TestRunner_Policy_ReportsTheLivePolicy(t *testing.T) {
	r := testRunner(t, &dialRecorder{})
	_, err := r.Start(context.Background(), Binding{
		SandboxID: "sb-live", TaskID: "MGIT-72",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy:    allowlistPolicy("a.example:443"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Stop("sb-live") })

	state, err := r.Policy("sb-live")
	require.NoError(t, err)
	assert.Equal(t, []string{"a.example:443"}, state.Entries)
	assert.Equal(t, 1, state.RuleCount)

	_, err = r.SetPolicy("sb-live", []string{"b.example:443", "c.example:443"}, false)
	require.NoError(t, err)

	state, err = r.Policy("sb-live")
	require.NoError(t, err)
	assert.Equal(t, []string{"b.example:443", "c.example:443"}, state.Entries)
	assert.Equal(t, 2, state.RuleCount)
}

// TestRunner_Policy_UnknownSandbox_FailsClosed verifies an unknown sandbox is
// an error, never an empty policy that would read as "nothing is allowed" and
// let a caller believe egress is closed when nothing is enforcing at all.
// Refs: MGIT-72, SEC-04
func TestRunner_Policy_UnknownSandbox_FailsClosed(t *testing.T) {
	r := testRunner(t, &dialRecorder{})

	_, err := r.Policy("sb-absent")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no running egress stack")
}

// TestRunner_SetPolicy_KillsFlowsOnTheTRANSPARENTPath is the assertion whose
// absence hid a live bug on the firecracker backend.
//
// There are TWO egress data paths in this runner, and only one of them is the
// one a guest uses. The length-prefixed CONNECT proxy is what the pre-existing
// kill tests drove — and nothing inside any guest speaks that protocol. An
// ordinary program (npm, apt, curl) is REDIRECTED into the TRANSPARENT proxy
// instead. That proxy was constructed without the flow registry, so every real
// guest connection was untracked: a revoke swapped the ruleset and reported
// killed=0 while the connection carrying data stayed open.
//
// That is the same shape as MGIT-68 and MGIT-69 — the enforcement was real,
// the tests drove a path no guest takes, and the gap was invisible because
// killed=0 reads identically to "there was nothing to kill".
//
// Refs: MGIT-72, MGIT-69, MGIT-68, SEC-04, ADR-012
func TestRunner_SetPolicy_KillsFlowsOnTheTRANSPARENTPath(t *testing.T) {
	dest, held := holdingTestDestination(t)
	r, eps := transparentRunner(t, dest)

	guest := dialTransparent(t, eps.TransparentAddr)
	requireSplicedBytes(t, guest, held)

	change, err := r.SetPolicy(transparentSandboxID, nil, false)

	require.NoError(t, err)
	assert.Positive(t, change.Killed,
		"a flow established through the TRANSPARENT path — the only path a real guest "+
			"takes — must be terminated by a revoke; killed=0 here means it was never tracked")
	assertConnClosed(t, guest)
}

// TestRunner_SetPolicy_Drain_SparesFlowsOnTheTRANSPARENTPath is the positive
// control for the test above: the same flow on the same path SURVIVES when
// draining was asked for, which is what makes the kill a decision rather than
// an artifact of the connection dying for some other reason.
func TestRunner_SetPolicy_Drain_SparesFlowsOnTheTRANSPARENTPath(t *testing.T) {
	dest, held := holdingTestDestination(t)
	r, eps := transparentRunner(t, dest)

	guest := dialTransparent(t, eps.TransparentAddr)
	requireSplicedBytes(t, guest, held)

	change, err := r.SetPolicy(transparentSandboxID, nil, true)

	require.NoError(t, err)
	assert.Zero(t, change.Killed, "a drained revoke terminates nothing")
	assert.True(t, change.Drained)
	assertConnAlive(t, guest)
}

// transparentSandboxID names the sandbox the transparent-path tests act on.
const transparentSandboxID = "sbx-transparent"

// transparentDestIP is the destination the transparent-path tests allowlist.
// It must be genuinely public: the authorizer denies loopback and RFC1918
// unconditionally (SEC-04/T9), so the ALLOWED flow is dialed to the local
// holding listener through the injected Dial while the POLICY still names a
// routable address.
var transparentDestIP = netip.MustParseAddr("140.82.112.4")

// transparentTestPort is the destination port the policy names.
const transparentTestPort = 443

// holdingTestDestination is a local stand-in for the allowlisted destination
// that KEEPS connections open, so nothing but the revoke can end the flow. It
// returns the listener and a channel carrying each accepted connection.
func holdingTestDestination(t *testing.T) (net.Listener, chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_, _ = c.Write([]byte("REAL-BYTES\n"))
			accepted <- c
		}
	}()
	return ln, accepted
}

// transparentRunner starts an allowlist sandbox whose TRANSPARENT listener is
// reachable directly, by injecting the OriginalDst the kernel's REDIRECT would
// otherwise supply. Everything else — the authorizer, the policy, the splice —
// is the production path.
func transparentRunner(t *testing.T, dest net.Listener) (*Runner, Endpoints) {
	t.Helper()
	r, err := NewRunner(RunnerConfig{
		Audit:  &fakeAuditor{},
		Lookup: resolvesTo(transparentDestIP.String()),
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", dest.Addr().String())
		},
		Clock:  frozenClock(),
		Logger: quietLogger(),
		// The seam the redirect would fill in. Without it this proxy would
		// read SO_ORIGINAL_DST off a connection nothing redirected.
		OriginalDst: func(net.Conn) (netip.AddrPort, error) {
			return netip.AddrPortFrom(transparentDestIP, transparentTestPort), nil
		},
		ProxyPort: 0, DNSPort: 0, TransparentPort: 0,
	})
	require.NoError(t, err)
	eps, err := r.Start(context.Background(), Binding{
		SandboxID: transparentSandboxID, TaskID: "MGIT-72",
		GatewayIP: netip.MustParseAddr("127.0.0.1"),
		Policy: allowlistPolicy(
			transparentDestIP.String() + ":" + strconv.Itoa(transparentTestPort)),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Stop(transparentSandboxID) })
	require.NotEmpty(t, eps.TransparentAddr, "the transparent listener must be bound")
	return r, eps
}

// dialTransparent opens a connection into the transparent listener, standing
// in for a guest program whose TCP the tap redirected there.
func dialTransparent(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// requireSplicedBytes asserts the flow reached the destination and carried
// real bytes back, so a later teardown is about an ESTABLISHED flow.
func requireSplicedBytes(t *testing.T, guest net.Conn, accepted chan net.Conn) {
	t.Helper()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the allowed flow never reached the destination, so nothing below proves anything")
	}
	require.NoError(t, guest.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 10)
	n, err := guest.Read(buf)
	require.NoError(t, err, "the flow must carry real bytes before the revoke")
	require.Positive(t, n)
}

// assertConnClosed asserts a connection has been torn down.
func assertConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue // draining buffered bytes; the close is what we are after
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("the established flow was NOT torn down by the revoke — it is still open")
		}
		return // EOF or reset: the flow is gone
	}
}

// assertConnAlive asserts a connection is still established: a read times out
// (nothing to read) rather than reporting the peer gone.
func assertConnAlive(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(750*time.Millisecond)))
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	if err == nil {
		return // real data: unambiguously alive
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return // idle but established
	}
	t.Fatalf("a DRAINED revoke must leave the established flow alone, but it ended: %v", err)
}
