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
//
// THE PRECONDITION IS GUEST-SIDE, and that is not a detail: both halves wait
// for the GUEST to report a flow carrying real bytes (heldflow_linux_test.go)
// before the revoke, because the destination's accept happens before the
// guest's first read and a revoke landing in that gap made the kill half fail
// on a precondition it had never met. Refs: MGIT-96, MGIT-72, MGIT-68, SEC-04, ADR-012
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
// It deliberately reports NOTHING about acceptance. It used to signal the
// first accept, and the suite waited on that before revoking — but an accepted
// connection is not yet a connection carrying data, so a revoke landing in the
// gap left the guest reporting CONNECTED-NO-DATA and failed the kill assertion
// on an unmet precondition. The wait now belongs to the guest's own
// establishment marker (heldFlow.awaitEstablished). Refs: MGIT-96
func holdingListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
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
	return ln
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

// livePolicyFixture is one launched sandbox ready for the live-policy
// assertions.
//
// workDir is the manager's state root, which is what locates each sandbox's
// per-VM vsock socket — the hold probe dials the guest through it directly so
// its output can be watched while it is still running (see streamProbe).
type livePolicyFixture struct {
	mgr       *microvm.Manager
	info      *model.SandboxInfo
	runner    *egress.Runner
	workDir   string
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
	target := holdingListener(t)
	mgr, ref, workDir := registerGuestManagerAt(t, kernel, rootfs, "")

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
		mgr: mgr, info: info, runner: runner, workDir: workDir,
		probePath: probePath, dest: dest,
	}
}

// control asserts the destination is reachable at all, so a later refusal or
// teardown is a decision rather than a broken sandbox.
//
// It runs through microvm.Manager.Exec deliberately: reaching the guest that
// way is also what proves the guest is serving its control channel, which is
// what lets the hold probe dial the exec channel directly afterwards without
// the manager's first-command retry (streamProbe).
func (f *livePolicyFixture) control(t *testing.T) {
	t.Helper()
	out := probe(t, f.mgr, f.info.ID, fmt.Sprintf("%s dial %s 2>&1", f.probePath, f.dest))
	require.Contains(t, out, "PROBE-RESULT DIAL = ALLOWED",
		"the allowlisted destination must be reachable BEFORE the revoke, or nothing "+
			"below proves anything")
}

// startHold launches the netprobe `hold` verb inside the guest against this
// fixture's destination. Refs: MGIT-96
func (f *livePolicyFixture) startHold(t *testing.T) *heldFlow {
	t.Helper()
	return startHeldFlow(t, f.workDir, f.info.ID, f.probePath, f.dest)
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
// holding listener is there to rule out).
//
// The revoke fires only once the GUEST reports the flow established and
// carrying real bytes, not merely accepted by the destination: revoking in
// that gap left the guest reporting CONNECTED-NO-DATA and failed this
// assertion on an unmet precondition. Refs: MGIT-96, MGIT-72, SEC-04, ADR-012
func TestE2E_Firecracker_Revoke_KillsEstablishedFlow(t *testing.T) {
	f := livePolicySandbox(t)
	f.control(t)

	held := f.startHold(t)
	held.awaitEstablished(t, f.info)

	change, err := f.runner.SetPolicy(f.info.ID, nil, false)
	require.NoError(t, err)
	t.Logf("revoke applied: rules=%d killed=%d drained=%v", change.RuleCount, change.Killed, change.Drained)

	assert.Positive(t, change.Killed,
		"the revoke reported killed=%d while a flow was demonstrably established — "+
			"either the flow was never tracked or the kill did not happen", change.Killed)
	assert.False(t, change.Drained, "a revoke without drain must not report drained")

	out := held.await(t)
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
// left alone.
//
// It waits on the SAME guest-side establishment marker as the kill half. It
// shared the weaker host-accept wait and merely got away with it — a drained
// flow is left running, so it always had time to read its bytes afterwards and
// still reported SURVIVED. A control that passes for a reason other than the
// one under test is not a control, and the two halves must differ only in the
// drain flag. Refs: MGIT-96, MGIT-72, SEC-04, ADR-012
func TestE2E_Firecracker_Revoke_DrainKeepsEstablishedFlow(t *testing.T) {
	f := livePolicySandbox(t)
	f.control(t)

	held := f.startHold(t)
	held.awaitEstablished(t, f.info)

	change, err := f.runner.SetPolicy(f.info.ID, nil, true)
	require.NoError(t, err)
	t.Logf("drained revoke applied: rules=%d killed=%d drained=%v",
		change.RuleCount, change.Killed, change.Drained)

	assert.Zero(t, change.Killed, "a drained revoke must terminate nothing")
	assert.True(t, change.Drained)

	out := held.await(t)
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
