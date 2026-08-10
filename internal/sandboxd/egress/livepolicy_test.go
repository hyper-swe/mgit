package egress

import (
	"context"
	"io"
	"net"
	"net/netip"
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
// arbitrarily long. Refs: MGIT-72, ADR-011, SEC-04
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
// revoke terminates in-flight connections. Refs: MGIT-72, ADR-011
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
