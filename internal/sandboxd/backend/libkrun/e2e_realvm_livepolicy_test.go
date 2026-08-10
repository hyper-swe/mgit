//go:build cgo && !vzf && (darwin || (linux && libkrun))

// REAL-VM ESTABLISHED-FLOW e2e for live policy revoke (MGIT-72).
//
// WHY THIS FILE EXISTS, stated plainly because its absence was the GA blocker:
// the first real-VM run of live policy revoke reported killed=0. Not because
// the kill path was broken, but because the probe closed its connection
// between calls, so there was never an established flow to terminate. killed=0
// is indistinguishable between "nothing to kill" and "kill is broken", and a
// check that can never fire proves nothing.
//
// Terminating the connection ALREADY CARRYING DATA is the whole meaning of
// "revoke means revoke": a revoke that swaps the ruleset but leaves the open
// socket running is exactly the exfiltration channel the caller just revoked.
//
// Both halves are asserted, on hardware:
//
//	KILL  (default)  — the held connection DIES, and the host counts killed>=1;
//	DRAIN (--drain)  — the held connection SURVIVES, and the host counts killed=0.
//
// The drain half is the POSITIVE CONTROL. Without it, a probe whose connection
// died for an unrelated reason (a server hang-up, a wedged guest) would read as
// a passing kill test. And BOTH halves additionally assert that a NEW flow is
// refused afterwards, so "survived" can never be a policy change that silently
// did nothing. Refs: MGIT-72, MGIT-74, SEC-04, ADR-012
package libkrun

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/backend/microvm"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
)

// holdWindowSeconds is how long the guest keeps its connection open after
// announcing it. Long enough that a surviving connection is a real
// observation rather than a short gap, short enough to stay inside the
// destination's keep-alive idle timeout so a server-side close does not
// masquerade as a kill.
const holdWindowSeconds = 10

// establishedMarker is the probe's "my connection is up and has carried real
// bytes" line. The host waits for it before mutating policy — a fixed sleep
// would race the handshake, which is how killed=0 happened.
const establishedMarker = "PROBE-HOLD ESTABLISHED"

// livePolicyVM boots a real microVM running mgit-guest in allowlist mode and
// returns everything the live-policy assertions need: the work dir (which
// locates the host-side control socket), the sandbox ID, and a probe runner.
func livePolicyVM(t *testing.T, allowlist []string) (workDir, sandboxID string, probe func(...string) string) {
	t.Helper()
	guestRoot := netProbeGuest(t)
	workDir = shortTempDir(t)
	sandboxID = "hold" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)

	cfg := realVMConfig(t, guestRoot, model.NetworkModeAllowlist, allowlist)
	cfg.SandboxID = sandboxID
	cfg.StateDir = microvm.SandboxStateDir(workDir, sandboxID)
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	cfg.RootfsReadOnly = false
	cfg.VsockEnabled = true

	vm, console := bootVMUntil(t, cfg, `"vsock_port":1024`)
	t.Logf("guest boot console:\n%s", console)
	t.Cleanup(func() { _ = vm.Stop(context.Background(), true) })

	return workDir, sandboxID, func(args ...string) string {
		t.Helper()
		out, err := execProbe(workDir, sandboxID, nil, args...)
		if err != nil {
			t.Fatalf("exec netprobe %v: %v (output=%q)", args, err, out)
		}
		t.Logf("netprobe %v ->\n%s", args, out)
		return out
	}
}

// execProbe runs one netprobe invocation in the guest, streaming its output
// through watch as it arrives. watch may be nil.
func execProbe(workDir, sandboxID string, watch *markerWatcher, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		return "", fmt.Errorf("host could not reach mgit-guest's exec channel: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var stderr bytes.Buffer
	sink := &bytes.Buffer{}
	// Each frame is written as it arrives, so the marker is observed the
	// moment the guest prints it rather than when the probe exits.
	stdout := writerFunc(func(p []byte) (int, error) {
		sink.Write(p)
		watch.observe(string(p))
		return len(p), nil
	})
	if _, err := guestexec.Run(conn, model.ExecRequest{
		Command: append([]string{"/bin/netprobe"}, args...),
	}, stdout, &stderr); err != nil {
		return sink.String() + stderr.String(), err
	}
	return sink.String() + stderr.String(), nil
}

// writerFunc adapts a function to io.Writer so the probe's stdout can be both
// accumulated and watched for the establishment marker as it streams.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// markerWatcher fires a channel the first time a marker appears in a stream.
// A nil watcher is a working no-op.
type markerWatcher struct {
	marker string
	once   sync.Once
	seen   chan struct{}
	mu     sync.Mutex
	buf    strings.Builder
}

func newMarkerWatcher(marker string) *markerWatcher {
	return &markerWatcher{marker: marker, seen: make(chan struct{})}
}

// observe feeds a chunk of stream output to the watcher.
func (w *markerWatcher) observe(chunk string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.buf.WriteString(chunk)
	hit := strings.Contains(w.buf.String(), w.marker)
	w.mu.Unlock()
	if hit {
		w.once.Do(func() { close(w.seen) })
	}
}

// heldFlow is a netprobe `hold` running in the guest: a connection that stays
// open across a policy mutation, so the mutation has something to terminate.
type heldFlow struct {
	watch *markerWatcher
	done  chan string
	out   string
	err   error
}

// startHeldFlow launches the hold probe and returns once it is RUNNING (not
// once it is established — use awaitEstablished for that).
func startHeldFlow(t *testing.T, workDir, sandboxID, dest string) *heldFlow {
	t.Helper()
	h := &heldFlow{watch: newMarkerWatcher(establishedMarker), done: make(chan string, 1)}
	go func() {
		out, err := execProbe(workDir, sandboxID, h.watch,
			"hold", dest, fmt.Sprintf("%d", holdWindowSeconds))
		h.err = err
		h.done <- out
	}()
	return h
}

// awaitEstablished blocks until the guest reports a live connection carrying
// real bytes, and FAILS if it never does — an unestablished flow would make
// every assertion after it vacuous, which is the exact defect this file fixes.
func (h *heldFlow) awaitEstablished(t *testing.T) {
	t.Helper()
	select {
	case <-h.watch.seen:
		t.Logf("guest reports an ESTABLISHED flow carrying real bytes; revoking now")
	case out := <-h.done:
		t.Fatalf("the hold probe exited before it established a connection, so there "+
			"was nothing for the revoke to kill — this is the killed=0 defect; got:\n%s", out)
	case <-time.After(60 * time.Second):
		t.Fatal("the hold probe never reported an established connection within 60s")
	}
}

// await returns the probe's full output once it finishes.
func (h *heldFlow) await(t *testing.T) string {
	t.Helper()
	select {
	case out := <-h.done:
		h.out = out
		if h.err != nil {
			t.Fatalf("hold probe failed: %v (output=%q)", h.err, out)
		}
		t.Logf("hold probe ->\n%s", out)
		return out
	case <-time.After(time.Duration(holdWindowSeconds+60) * time.Second):
		t.Fatal("the hold probe never finished")
		return ""
	}
}

// assertNewFlowRefusedByPolicy is the guard that keeps "the held connection
// survived" from being a policy change that did nothing at all: after ANY
// revoke — killing or draining — the NEXT flow must be refused, and refused by
// POLICY rather than by a dead network.
//
// The refusal's observable differs by backend by construction (libkrun's
// in-process forwarder resets the handshake, which the guest sees as
// "connection refused"), so this asserts the CLASS of failure, not one errno.
func assertNewFlowRefusedByPolicy(t *testing.T, probe func(...string) string, dest string) {
	t.Helper()
	out := probe("dial", dest)
	if strings.Contains(out, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("a NEW flow was still admitted after the revoke, so the policy change "+
			"did not take effect; got: %s", out)
	}
	if strings.Contains(out, "network is unreachable") || strings.Contains(out, "no route to host") {
		t.Fatalf("the new flow failed because the guest network DIED, not because policy "+
			"refused it — a revoke that breaks the stack is not a revoke; got: %s", out)
	}
	if !strings.Contains(out, "connection refused") && !strings.Contains(out, "connection reset") {
		t.Errorf("denial reason %q is neither a reset nor a refusal; a policy denial must "+
			"be recognizable as a decision (SEC-04)", out)
	}
}

// TestE2E_Libkrun_RealVM_Revoke_KillsEstablishedFlow is the GA-blocking
// assertion: a connection that is OPEN AND CARRYING DATA when the revoke lands
// is terminated by it.
//
// It is the assertion killed=0 could not make. Both ends are checked — the
// host's count of what it killed, and the guest's own observation that its
// socket died — because either alone can lie: a count with no guest-side death
// would be bookkeeping, and a guest-side death with no count could be the
// server hanging up. Refs: MGIT-72, SEC-04, ADR-012
func TestE2E_Libkrun_RealVM_Revoke_KillsEstablishedFlow(t *testing.T) {
	requireRealVM(t)
	ips := requireHostInternet(t)
	dest := net.JoinHostPort(ips[0], fmt.Sprintf("%d", netE2EPort))

	workDir, sandboxID, probe := livePolicyVM(t, []string{dest})

	// Control: the destination is reachable at all, so a later refusal is a
	// decision rather than a broken sandbox.
	if before := probe("dial", dest); !strings.Contains(before, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the destination must be reachable BEFORE the revoke, or nothing "+
			"below proves anything; got: %s", before)
	}

	held := startHeldFlow(t, workDir, sandboxID, dest)
	held.awaitEstablished(t)

	// THE REVOKE. drain=false: established flows are killed (ADR-012).
	resp, err := NewPolicyClient(workDir, sandboxID).SetPolicy(nil, false)
	if err != nil {
		t.Fatalf("live policy revoke failed: %v", err)
	}
	t.Logf("revoke applied: rules=%d killed=%d drained=%v", resp.Rules, resp.Killed, resp.Drained)

	if resp.Killed < 1 {
		t.Fatalf("the revoke reported killed=%d while a flow was demonstrably established "+
			"— either the flow was never tracked or the kill did not happen; this is "+
			"exactly the ambiguity killed=0 left open", resp.Killed)
	}
	if resp.Drained {
		t.Errorf("a revoke without --drain must not report drained=true")
	}

	out := held.await(t)
	if !strings.Contains(out, "PROBE-RESULT HOLD = DIED") {
		t.Fatalf("the host counted killed=%d but the GUEST's established connection did "+
			"not die — the kill did not reach the flow that was carrying data; got:\n%s",
			resp.Killed, out)
	}

	// And the policy really changed, not just the sockets.
	assertNewFlowRefusedByPolicy(t, probe, dest)
	t.Logf("REAL VM PASS (kill): an established, data-carrying flow was TERMINATED by "+
		"the revoke (host killed=%d) and the next flow was refused by policy\n%s",
		resp.Killed, strings.TrimSpace(out))
}

// TestE2E_Libkrun_RealVM_Revoke_DrainKeepsEstablishedFlow is the POSITIVE
// CONTROL for the test above: with --drain, the same held connection SURVIVES
// the same revoke.
//
// Without this, a hold probe whose connection died for an unrelated reason
// would read as a passing kill test. With it, the kill is shown to be a
// DECISION: same VM shape, same probe, same revoke, opposite outcome, selected
// only by the drain flag.
//
// The trailing new-flow assertion is what stops "survived" from being an
// accidental no-op: the ruleset must have changed even though the sockets were
// left alone. Refs: MGIT-72, SEC-04, ADR-012
func TestE2E_Libkrun_RealVM_Revoke_DrainKeepsEstablishedFlow(t *testing.T) {
	requireRealVM(t)
	ips := requireHostInternet(t)
	dest := net.JoinHostPort(ips[0], fmt.Sprintf("%d", netE2EPort))

	workDir, sandboxID, probe := livePolicyVM(t, []string{dest})

	if before := probe("dial", dest); !strings.Contains(before, "PROBE-RESULT DIAL = ALLOWED") {
		t.Fatalf("the destination must be reachable BEFORE the revoke; got: %s", before)
	}

	held := startHeldFlow(t, workDir, sandboxID, dest)
	held.awaitEstablished(t)

	// THE SAME REVOKE, drained.
	resp, err := NewPolicyClient(workDir, sandboxID).SetPolicy(nil, true)
	if err != nil {
		t.Fatalf("live policy revoke (drain) failed: %v", err)
	}
	t.Logf("drained revoke applied: rules=%d killed=%d drained=%v", resp.Rules, resp.Killed, resp.Drained)

	if resp.Killed != 0 {
		t.Errorf("a --drain revoke must terminate nothing; it reported killed=%d", resp.Killed)
	}
	if !resp.Drained {
		t.Errorf("a --drain revoke must report drained=true; got %+v", resp)
	}

	out := held.await(t)
	if !strings.Contains(out, "PROBE-RESULT HOLD = SURVIVED") {
		t.Fatalf("with --drain the established flow must be LEFT ALONE, but it did not "+
			"survive the revoke; got:\n%s", out)
	}

	// The decisive guard: the ruleset really changed, so SURVIVED means
	// "deliberately spared", not "the revoke did nothing".
	assertNewFlowRefusedByPolicy(t, probe, dest)
	t.Logf("REAL VM PASS (drain control): the SAME held flow survived the SAME revoke "+
		"under --drain (killed=0) while the next flow was refused by policy\n%s",
		strings.TrimSpace(out))
}
