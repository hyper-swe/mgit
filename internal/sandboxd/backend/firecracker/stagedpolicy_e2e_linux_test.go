//go:build linux

// REAL-VM proof that a policy staged onto a REGISTERED-but-unbooted firecracker
// sandbox is the policy its guest boots enforcing (MGIT-156, the live half of
// MGIT-109).
//
// WHAT THE UNIT TESTS DO NOT COVER. MGIT-109 moved the staging decision into
// the service, above the backend: `policy set` against a `created` sandbox
// replaces the allowlist on its pending launch instead of dialing a control
// socket that does not exist. That is backend-independent by construction and
// unit-tested with a fake controller. What no test exercised on this backend
// is everything between "the service stored it" and "the guest cannot reach an
// off-list destination": the staged list has to ride the pending launch
// options, reach the SandboxInfo the boot hands the egress adapter, and be the
// list the proxy and the restricted DNS bind on the tap gateway.
//
// THE OBSERVATION IS FROM INSIDE THE GUEST, and the staged list DIFFERS from
// the launch-time one on purpose: the sandbox is registered allowing
// `stale.test` and staged to allow `allowed.test` instead. A guest enforcing
// the staged list resolves `allowed.test` and is REFUSED `stale.test`; a guest
// that booted with the launch-time list — which is exactly what a broken
// staging path produces — passes neither assertion. The host's own view of
// its rules is read back only after the guest has been observed.
//
// THE WIRING IS THE DAEMON'S. The service, the policy service and the two
// egress adapters are the production types; only the host policy reader, the
// event sink and the id generator are stand-ins, and the egress runner's
// lookup and dial are hermetic (a local listener stands in for the public
// destination), as in the allowlist proof beside this file.
// Refs: MGIT-156, MGIT-109, MGIT-72, FR-17.7, FR-17.8, FR-17.10, SEC-04
package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/service"
)

const (
	stagedTask         = "MGIT-156"
	stagedLaunchEntry  = "stale.test"   // what the sandbox is REGISTERED to allow
	stagedPendingEntry = "allowed.test" // what is STAGED onto it before boot
)

// stagedEventSink records every sandbox event the service and the policy
// service append, so the audit trail's phases can be asserted.
type stagedEventSink struct {
	mu     sync.Mutex
	events []model.SandboxEvent
}

func (s *stagedEventSink) AppendSandboxEvent(_ context.Context, ev *model.SandboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, *ev)
	return nil
}

// policyPhases returns the `phase` of every policy-changed event, in order,
// with "+pending" appended where the record says the policy is not in force.
func (s *stagedEventSink) policyPhases(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var phases []string
	for _, ev := range s.events {
		if ev.EventType != model.EventPolicyChanged {
			continue
		}
		var d struct {
			Phase   string `json:"phase"`
			Pending bool   `json:"pending"`
		}
		require.NoError(t, json.Unmarshal([]byte(ev.Detail), &d), "policy event detail is JSON")
		if d.Pending {
			d.Phase += "+pending"
		}
		phases = append(phases, d.Phase)
	}
	return phases
}

// defaultHostPolicy is the host policy reader: the shipped defaults.
type defaultHostPolicy struct{}

func (defaultHostPolicy) Load(context.Context) (model.SandboxPolicy, error) {
	return model.DefaultSandboxPolicy(), nil
}

// stagedRunner builds the egress runner exactly as the allowlist proof does:
// the staged name resolves to the TEST-NET address, anything else is NXDOMAIN,
// and authorized flows dial a local stand-in target.
func stagedRunner(t *testing.T) (*egress.Runner, *recordingEgressAudit) {
	t.Helper()
	target, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			c, aerr := target.Accept()
			if aerr != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
		}
	}()
	audit := &recordingEgressAudit{}
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit: audit,
		Lookup: func(_ context.Context, name string) ([]netip.Addr, error) {
			if name == stagedPendingEntry {
				return []netip.Addr{allowedTestIP}, nil
			}
			return nil, egress.ErrNXDOMAIN
		},
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		Clock:     func() time.Time { return time.Now().UTC() },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProxyPort: hostProxyPort,
		DNSPort:   hostDNSPort,
	})
	require.NoError(t, err)
	return runner, audit
}

// serviceProbe execs one shell probe in the guest THROUGH THE SERVICE — the
// first call is the sandbox's first use and boots the VM — retrying until the
// guest serves vsock. It returns stdout+stderr.
func serviceProbe(t *testing.T, svc *service.SandboxService, script string) string {
	t.Helper()
	var (
		res *model.ExecResult
		err error
	)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		res, err = svc.Exec(context.Background(), stagedTask, model.ExecRequest{
			Command: []string{"/bin/sh", "-c", script},
		})
		if err == nil {
			return string(res.Stdout) + string(res.Stderr)
		}
		time.Sleep(400 * time.Millisecond)
	}
	require.NoError(t, err, "exec must reach the guest once it serves vsock")
	return ""
}

// proxyConnect drives the gateway-bound proxy the guest reaches and reports
// whether it authorizes a CONNECT to host:443.
func proxyConnect(t *testing.T, gw netip.Addr, host string) bool {
	t.Helper()
	//nolint:gosec // G704: the address is the sandbox's own tap gateway, derived from the host-owned sandbox ID — never input
	c, err := net.DialTimeout("tcp", net.JoinHostPort(gw.String(), strconv.Itoa(hostProxyPort)), 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	require.NoError(t, egress.EncodeConnectRequest(c, egress.ConnectRequest{Protocol: "tcp", Host: host, Port: 443}))
	allow, _, err := egress.DecodeConnectReply(c)
	require.NoError(t, err)
	return allow
}

func TestE2E_Firecracker_StagedPolicy_CreatedSandboxBootsEnforcingIt(t *testing.T) {
	kernel, _ := requireKVM(t)
	requireNetRoot(t)
	rootfs := os.Getenv("MGIT_E2E_GUEST_ROOTFS")
	if rootfs == "" || !fileExists(rootfs) {
		t.Skip("set MGIT_E2E_GUEST_ROOTFS to a present guest image")
	}
	ctx := context.Background()
	runner, _ := stagedRunner(t)
	mgr, ref := registerGuestManager(t, kernel, rootfs, "")
	events := &stagedEventSink{}
	clock := func() time.Time { return time.Now().UTC() }
	newID := func() (string, error) { return ulid.Make().String(), nil }

	// The daemon's wiring: service + the backend's egress adapter at boot, the
	// policy service over the live enforcer with the service as the stager.
	svc, err := service.NewSandboxService(mgr, events, defaultHostPolicy{}, clock, newID)
	require.NoError(t, err)
	svc.SetEgressController(NewEgressController(runner))
	policySvc, err := service.NewEgressPolicyService(NewPolicyController(runner), svc, events, clock)
	require.NoError(t, err)

	// 1. Register with `--network allowlist` and do NOT boot: state is created,
	//    and nothing enforces anything yet (no proxy bound for this sandbox).
	wt := filepath.Join(t.TempDir(), "repo", "wt")
	require.NoError(t, os.MkdirAll(wt, 0o750))
	created, err := svc.Register(ctx, model.SandboxLaunchOptions{
		TaskID: stagedTask, WorktreePath: wt, ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeAllowlist, Allowlist: []string{stagedLaunchEntry}},
		CPUs:    1, MemoryMB: 256,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Remove(ctx, stagedTask, true) })
	require.Equal(t, model.StateCreated, created.State, "registration does not boot (lazy provisioning)")
	require.False(t, runner.Running(created.ID), "nothing enforces a policy for a sandbox that has not booted")

	// 2. `policy set` against the created sandbox reports PENDING: staged, not
	//    applied — and still nothing is bound.
	change, err := policySvc.Set(ctx, *created, []string{stagedPendingEntry}, false)
	require.NoError(t, err)
	assert.True(t, change.Pending, "a policy set on an unbooted sandbox is staged, not applied")
	assert.Equal(t, []string{stagedPendingEntry}, change.Entries)
	assert.Equal(t, 0, change.RuleCount, "nothing has compiled a pending policy")
	assert.False(t, runner.Running(created.ID), "staging binds no proxy: nothing enforces yet")
	shown, err := policySvc.Show(ctx, *created)
	require.NoError(t, err)
	assert.True(t, shown.Pending, "show reports the staged policy as pending")
	assert.Equal(t, []string{stagedPendingEntry}, shown.Entries)

	// 3. FIRST USE boots the VM. The guest is observed enforcing the STAGED
	//    list: the staged name resolves through the gateway DNS, the
	//    launch-time name — which a broken staging path would have booted
	//    with — is REFUSED. busybox nslookup exits 0 on REFUSED, so the text
	//    is asserted, not the exit code.
	gw, _, _ := subnetFor(created.ID)
	up := netUpPrefix(created.ID)
	okDNS := serviceProbe(t, svc, up+"nslookup "+stagedPendingEntry+" "+gw.String()+" 2>&1")
	assert.Contains(t, okDNS, allowedTestIP.String(), "the STAGED name resolves through the host DNS on the gateway")
	staleDNS := serviceProbe(t, svc, up+"nslookup "+stagedLaunchEntry+" "+gw.String()+" 2>&1")
	assert.NotContains(t, staleDNS, allowedTestIP.String(), "the launch-time name must not resolve")
	assert.Contains(t, staleDNS, "REFUSED", "the launch-time name is REFUSED: the guest booted with the staged list, not the launch-time one")
	assert.True(t, proxyConnect(t, gw, stagedPendingEntry), "the proxy authorizes a CONNECT to the staged name")
	assert.False(t, proxyConnect(t, gw, stagedLaunchEntry), "the proxy refuses a CONNECT to the launch-time name")

	// 4. Read the policy back after boot: in force (not pending), and exactly
	//    what was staged.
	status, err := svc.Status(ctx, stagedTask)
	require.NoError(t, err)
	require.Equal(t, model.StateRunning, status.State, "first use booted the VM")
	require.True(t, runner.Running(status.ID), "the egress stack is bound for the booted sandbox")
	live, err := policySvc.Show(ctx, *status)
	require.NoError(t, err)
	assert.False(t, live.Pending, "after boot the policy is in force")
	assert.Equal(t, []string{stagedPendingEntry}, live.Entries, "what boots is what was staged")
	assert.Positive(t, live.RuleCount, "the staged list compiled to at least one rule")

	// 5. The audit trail says staged, and never claims an application that did
	//    not happen: the boot applied it, no `applied` record was written.
	phases := events.policyPhases(t)
	assert.Equal(t, []string{"requested", "staged+pending"}, phases,
		"policy audit: a request, then a staged (pending) record — and no `applied` claim for a sandbox the boot configured")

	t.Logf("STAGED POLICY REAL VM PASS: registered allowing %q, staged %q before boot; guest resolved %q (%s) and was REFUSED %q; read-back after boot = %v (rules=%d)",
		stagedLaunchEntry, stagedPendingEntry, stagedPendingEntry, allowedTestIP, stagedLaunchEntry, live.Entries, live.RuleCount)
}
