//go:build linux

// REAL-VM ESTABLISHED-FLOW e2e for live policy revoke on the FIRECRACKER
// backend (MGIT-72).
//
// WHY A SECOND BACKEND'S PROOF IS NOT OPTIONAL. libkrun and firecracker
// enforce egress by completely different mechanisms: libkrun terminates the
// guest's TCP in a userspace netstack inside a re-exec'd VM child, while
// firecracker steers the guest through a tap + iptables REDIRECT into a
// host-side transparent proxy in the daemon's own process. The FlowRegistry is
// shared, but what it holds and when it is populated is not — a pass on one is
// not evidence for the other.
//
// Both halves are asserted, as on libkrun:
//
//	KILL  (default)  — the held connection DIES,     host reports killed>=1;
//	DRAIN (--drain)  — the held connection SURVIVES, host reports killed=0.
//
// The drain half is the POSITIVE CONTROL that makes the kill half a decision
// rather than a coincidence — without it, a connection that died for an
// unrelated reason (the destination hanging up, a wedged guest) reads as a
// passing kill test.
//
// DENIAL LOOKS DIFFERENT HERE BY CONSTRUCTION and no assertion hardcodes one
// errno: firecracker's redirect completes a TCP handshake with the local proxy
// and the proxy then resets, so the guest sees connect-then-reset, whereas
// libkrun's in-stack forwarder refuses at connect. The assertions are on the
// CLASS of outcome — reached-and-refused vs. never-reached — not on a string.
// Refs: MGIT-72, MGIT-68, SEC-04, ADR-012
package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// fcHoldSeconds is how long the guest keeps its connection open after it is
// established. A killed flow ends well inside this; a spared one runs the
// whole window, and the gap between the two is what the assertion reads.
const fcHoldSeconds = 20

// holdingListener stands in for the allowlisted destination and, unlike
// bannerListener, KEEPS EVERY CONNECTION OPEN for the test's duration.
//
// That is the whole point: a destination that hangs up after replying would
// end the guest's connection on its own, and the kill assertion would then
// pass without the revoke having done anything. Holding the connection makes
// the host the only thing that can end it.
//
// accepted fires on the first connection — the HOST-side proof that a flow is
// established, which is what the test waits for before revoking. A fixed sleep
// would race the handshake, which is how killed=0 happened on libkrun.
func holdingListener(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan struct{}, 1)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.WriteString(c, guestBannerText+"\n")
				// Hold it. The connection ends only when the host kills it or
				// the test finishes.
				<-done
			}(c)
		}
	}()
	return ln, accepted
}

// guestHold runs the netprobe `hold` verb INSIDE the guest: it opens a
// connection, carries real bytes on it, and then keeps it open for the window,
// reporting whether it DIED or SURVIVED.
//
// It is a real binary rather than a busybox shell pipeline for a reason worth
// recording: the first version of this helper ran `sleep N | nc ...` and timed
// the pipeline. A shell waits for EVERY member of a pipeline, so the elapsed
// time was N whether the connection was killed at once or never touched — an
// assertion that could not fail in the drain direction and could not pass in
// the kill direction. A probe that reports its own observation has no such
// ambiguity.
//
// It is the SAME probe the libkrun suite uses, so the two backends' proofs are
// stated in identical terms and can be compared directly.
func guestHold(t *testing.T, mgr *microvm.Manager, id, probePath, dest string) chan string {
	t.Helper()
	out := make(chan string, 1)
	script := fmt.Sprintf("%s hold %s %d 2>&1", probePath, dest, fcHoldSeconds)
	go func() { out <- probe(t, mgr, id, script) }()
	return out
}

// buildNetProbe cross-compiles the guest probe into the worktree the sandbox
// will mount at its identical path, so the guest can exec it.
//
// The source is the libkrun suite's testdata probe, used unchanged: one probe
// for both backends means the kill/drain proofs are the same observation made
// twice, not two observations that merely sound alike.
func buildNetProbe(t *testing.T, outPath string) {
	t.Helper()
	// A prebuilt probe may be supplied when the toolchain cannot run in the
	// test's environment (e.g. a snap-packaged Go inside a user namespace).
	// It is the SAME binary built from the SAME source — only the moment of
	// the build moves.
	if prebuilt := os.Getenv("MGIT_E2E_NETPROBE"); prebuilt != "" {
		data, err := os.ReadFile(prebuilt) //nolint:gosec // test-owned path
		require.NoError(t, err, "read the prebuilt guest probe %s", prebuilt)
		require.NoError(t, os.WriteFile(outPath, data, 0o755)) //nolint:gosec // must be executable in the guest
		return
	}
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate the test source to find the shared netprobe")
	src := filepath.Join(filepath.Dir(thisFile), "..", "libkrun", "testdata", "netprobe")
	//nolint:gosec // G204: fixed argv; every path is test-owned
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", outPath, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	combined, err := cmd.CombinedOutput()
	require.NoError(t, err, "build the guest probe: %s", combined)
}

// awaitHeldFlow blocks until the DESTINATION reports a connection, and fails
// if none arrives — an unestablished flow makes every assertion after it
// vacuous, which is exactly the defect this file exists to close.
func awaitHeldFlow(t *testing.T, accepted <-chan struct{}, info *model.SandboxInfo) {
	t.Helper()
	select {
	case <-accepted:
		t.Log("the destination reports an ESTABLISHED flow from the guest; revoking now")
	case <-time.After(20 * time.Second):
		t.Fatalf("the guest never reached the destination, so there is nothing for the "+
			"revoke to kill — this is the killed=0 defect.\n%s", hostNetDiagnostics(t, info))
	}
}

// heldOutcome returns the hold probe's full output once it finishes.
func heldOutcome(t *testing.T, out chan string) string {
	t.Helper()
	select {
	case got := <-out:
		t.Logf("hold probe ->\n%s", got)
		return got
	case <-time.After(time.Duration(fcHoldSeconds+90) * time.Second):
		t.Fatal("the hold probe never finished")
		return ""
	}
}

// livePolicyFixture is one launched sandbox ready for the live-policy
// assertions.
type livePolicyFixture struct {
	mgr       *microvm.Manager
	info      *model.SandboxInfo
	runner    *egress.Runner
	accepted  <-chan struct{}
	probePath string
	dest      string
}

// livePolicySandbox boots an allowlist sandbox whose authorized flows are
// dialed to the HOLDING destination, with the guest probe delivered in the
// worktree the guest mounts at its identical path.
func livePolicySandbox(t *testing.T) *livePolicyFixture {
	t.Helper()
	kernel, rootfs := requireGuestImage(t)
	requireNetRoot(t)

	// Allowlisted BY IP, not by name. This suite is about live policy
	// mutation and established flows; the name/pinning path is MGIT-69's
	// subject and has its own tests. Naming the address directly keeps this
	// proof independent of the guest image's resolver, so a failure here
	// means the policy path and not DNS.
	dest := allowedTestIP.String() + ":" + strconv.Itoa(fcDestPort)
	target, accepted := holdingListener(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")

	wtPath := filepath.Join(t.TempDir(), "repo", "wt")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))
	probePath := filepath.Join(wtPath, "netprobe")
	buildNetProbe(t, probePath)

	info, err := mgr.Launch(context.Background(), model.SandboxLaunchOptions{
		TaskID:       "MGIT-72",
		WorktreePath: wtPath,
		ImageRef:     ref,
		Network:      model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{dest}},
		CPUs:         1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Remove(context.Background(), info.ID, true) })

	_, runner := startEgressRunnerFor(t, info, model.NetworkModeAllowlist, []string{dest}, target)
	requireGuestIsAddressed(t, mgr, info)
	return &livePolicyFixture{
		mgr: mgr, info: info, runner: runner, accepted: accepted,
		probePath: probePath, dest: dest,
	}
}

// control asserts the destination is reachable at all, so a later refusal or
// teardown is a decision rather than a broken sandbox — and then DRAINS the
// accepted signal it just produced.
//
// Draining is not bookkeeping: the accepted channel is buffered, so the
// control connection's signal would otherwise satisfy the wait for the HELD
// flow, and the revoke would fire before the held connection existed. That is
// the killed=0 race in a different costume.
func (f *livePolicyFixture) control(t *testing.T) {
	t.Helper()
	out := probe(t, f.mgr, f.info.ID, fmt.Sprintf("%s dial %s 2>&1", f.probePath, f.dest))
	require.Contains(t, out, "PROBE-RESULT DIAL = ALLOWED",
		"the allowlisted destination must be reachable BEFORE the revoke, or nothing "+
			"below proves anything")
	select {
	case <-f.accepted:
	default:
	}
}

// fcDestPort is the destination port the policy names.
const fcDestPort = 443

// TestE2E_Firecracker_Revoke_KillsEstablishedFlow is the kill assertion on
// this backend's data path: a connection that is OPEN AND CARRYING DATA when
// the revoke lands is terminated by it.
//
// Both ends are checked — the host's count of what it killed, and the guest's
// own observation that its connection ended early — because either alone can
// lie: a count with no guest-side death would be bookkeeping, and an early
// guest exit with no count could be the destination hanging up (which the
// holding listener is there to rule out). Refs: MGIT-72, SEC-04, ADR-012
func TestE2E_Firecracker_Revoke_KillsEstablishedFlow(t *testing.T) {
	f := livePolicySandbox(t)
	f.control(t)

	held := guestHold(t, f.mgr, f.info.ID, f.probePath, f.dest)
	awaitHeldFlow(t, f.accepted, f.info)

	change, err := f.runner.SetPolicy(f.info.ID, nil, false)
	require.NoError(t, err)
	t.Logf("revoke applied: rules=%d killed=%d drained=%v", change.RuleCount, change.Killed, change.Drained)

	assert.Positive(t, change.Killed,
		"the revoke reported killed=%d while a flow was demonstrably established — "+
			"either the flow was never tracked or the kill did not happen", change.Killed)
	assert.False(t, change.Drained, "a revoke without drain must not report drained")

	out := heldOutcome(t, held)
	assert.Contains(t, out, "PROBE-RESULT HOLD = DIED",
		"the host counted killed=%d but the GUEST's established connection did not die — "+
			"the kill did not reach the flow that was carrying data", change.Killed)

	assertFirecrackerFlowRefused(t, f)
	t.Logf("REAL VM PASS (firecracker kill): an established, data-carrying flow was "+
		"TERMINATED by the revoke (host killed=%d) and the next flow was refused\n%s",
		change.Killed, strings.TrimSpace(out))
}

// TestE2E_Firecracker_Revoke_DrainKeepsEstablishedFlow is the POSITIVE CONTROL
// for the test above: with drain, the same held connection SURVIVES the same
// revoke.
//
// The trailing new-flow assertion is what stops "survived" from being an
// accidental no-op: the ruleset must have changed even though the sockets were
// left alone. Refs: MGIT-72, SEC-04, ADR-012
func TestE2E_Firecracker_Revoke_DrainKeepsEstablishedFlow(t *testing.T) {
	f := livePolicySandbox(t)
	f.control(t)

	held := guestHold(t, f.mgr, f.info.ID, f.probePath, f.dest)
	awaitHeldFlow(t, f.accepted, f.info)

	change, err := f.runner.SetPolicy(f.info.ID, nil, true)
	require.NoError(t, err)
	t.Logf("drained revoke applied: rules=%d killed=%d drained=%v",
		change.RuleCount, change.Killed, change.Drained)

	assert.Zero(t, change.Killed, "a drained revoke must terminate nothing")
	assert.True(t, change.Drained)

	out := heldOutcome(t, held)
	assert.Contains(t, out, "PROBE-RESULT HOLD = SURVIVED",
		"with drain the established flow must be LEFT ALONE, but it did not survive")

	// The decisive guard: the ruleset really changed, so "survived" means
	// deliberately spared and not "the revoke did nothing".
	assertFirecrackerFlowRefused(t, f)
	t.Logf("REAL VM PASS (firecracker drain control): the SAME held flow survived the SAME "+
		"revoke under drain (killed=0) while the next flow was refused\n%s",
		strings.TrimSpace(out))
}

// assertFirecrackerFlowRefused checks that a NEW flow is refused after a
// revoke, and refused because policy said so rather than because the network
// died.
//
// The observable is deliberately backend-specific and stated as such: on this
// backend the guest's SYN is redirected to the host proxy, so the handshake
// COMPLETES and the proxy then resets — the guest never sees "connection
// refused" at connect the way it does on libkrun. What both backends share is
// that the flow reaches the enforcement point and carries no bytes, and that
// is what this asserts. "no route to host" would mean a dead stack and fails.
func assertFirecrackerFlowRefused(t *testing.T, f *livePolicyFixture) {
	t.Helper()
	out := probe(t, f.mgr, f.info.ID, fmt.Sprintf("%s dial %s 2>&1", f.probePath, f.dest))
	if strings.Contains(out, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("a NEW flow still reached the destination after the revoke, so the policy "+
			"change did not take effect; got: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "network is unreachable") ||
		strings.Contains(strings.ToLower(out), "no route to host") {
		t.Fatalf("the new flow failed because the guest NETWORK died, not because policy "+
			"refused it — a revoke that breaks the stack is not a revoke; got: %q", out)
	}
	t.Logf("post-revoke flow refused (backend observable: %q)", strings.TrimSpace(out))
}
