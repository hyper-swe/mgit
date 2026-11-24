//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// reviewEgressBody is what the stand-in destination serves. The reviewer must
// print these exact bytes: they prove the flow went through the host stack to
// the destination, not that a socket merely opened.
const reviewEgressBody = "REVIEW-EGRESS-REACHED-THE-GRANTED-DESTINATION"

// reviewEgressStack is the daemon's view of the stack, as the Linux wiring
// builds it: the controller starts and stops, the runner grants and revokes.
type reviewEgressStack struct {
	EgressController
	*egress.Runner
}

// grantingBindings stands in for the binder: it grants one exact ip:port
// through the seam the run hands it, and records what it was given.
type grantingBindings struct {
	entry string

	mu       sync.Mutex
	guestIDs []string
	hadGrant []bool
}

func (b *grantingBindings) Attach(ctx context.Context, in reviewsvc.AttachInput) (func(context.Context) error, error) {
	b.mu.Lock()
	b.guestIDs = append(b.guestIDs, in.GuestID)
	b.hadGrant = append(b.hadGrant, in.AllowEgress != nil)
	b.mu.Unlock()
	release := func(context.Context) error { return nil }
	if in.Mapping.EgressPolicyID == "none" {
		return release, nil
	}
	if in.AllowEgress == nil {
		return release, errors.New("the guest has no network")
	}
	return release, in.AllowEgress(ctx, b.entry)
}

func (b *grantingBindings) last() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.guestIDs[len(b.guestIDs)-1], b.hadGrant[len(b.hadGrant)-1]
}

// httpStandIn serves reviewEgressBody over plain HTTP and counts connections:
// the count is the host-side proof of whether a flow got through.
func httpStandIn(t *testing.T) (net.Listener, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n"+reviewEgressBody+"\n")
			}()
		}
	}()
	return ln, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// TestE2E_ReviewRun_EgressReachesOnlyTheGrantedDestination runs reviews on a
// real firecracker guest through the run service with the host egress stack
// wired as the daemon wires it. A mapping that names an egress policy gets an
// allowlist-mode guest with an empty list and a grant function; the binder
// grants one exact ip:port; the reviewer reaches that destination and nothing
// else (neither another port on that address nor another address on that
// port); and when the run ends, the stack is stopped and the guest's tap is
// gone. A mapping with no policy gets no grant function and no network. A
// grant the host refuses refuses the review before the reviewer starts.
// Refs: MGIT-344, FEAT-3.347.1, SEC-05
func TestE2E_ReviewRun_EgressReachesOnlyTheGrantedDestination(t *testing.T) {
	kernel, rootfs := requireE2EGuest(t)
	requireNetRoot(t)
	clock := func() time.Time { return time.Now().UTC() }
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	binder := sandboxd.NewPeerBinder(logger)
	mgr, ref, _ := e2eBootGuest(t, kernel, rootfs, binder, clock, logger)
	ctx := context.Background()

	probe, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		TaskID: "REVIEW-DIGEST", WorktreePath: t.TempDir(), ImageRef: ref,
		Network: model.NetworkPolicy{Mode: model.NetworkModeNone}, CPUs: 1, MemoryMB: 256, NoGuestRepo: true,
	})
	require.NoError(t, err)
	sum, err := mgr.Exec(ctx, probe.ID, model.ExecRequest{Command: []string{"/bin/busybox", "sha256sum", "/bin/busybox"}, Timeout: 60 * time.Second})
	require.NoError(t, err)
	require.Zero(t, sum.ExitCode, string(sum.Stderr))
	digest := strings.Fields(string(sum.Stdout))[0]
	require.NoError(t, mgr.Remove(ctx, probe.ID, true))

	// The stack, hermetic: no name resolves, and every authorized flow is
	// dialed to the local stand-in whatever its destination.
	target, connections := httpStandIn(t)
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit: &recordingEgressAudit{},
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return nil, egress.ErrNXDOMAIN
		},
		Dial: func(ctx context.Context, _ netip.Addr, _ int) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", target.Addr().String())
		},
		Clock:           clock,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProxyPort:       hostProxyPort,
		DNSPort:         hostDNSPort,
		TransparentPort: hostTransparentPort,
	})
	require.NoError(t, err)

	hostRoot := t.TempDir()
	require.NoError(t, attest.GenerateKey(ctx, hostRoot, reviewKeyAudit{}))
	svc, err := reviewsvc.New(hostRoot, clock)
	require.NoError(t, err)
	granted := allowedTestIP.String() + ":443"
	bind := &grantingBindings{entry: granted}
	svc.WithRunner(reviewsvc.RunnerConfig{
		Manager: mgr, Backend: model.BackendKVM, Bindings: bind,
		Egress: reviewEgressStack{EgressController: NewEgressController(runner), Runner: runner},
	})

	install := func(id, policy string, argv ...string) {
		raw, _ := json.Marshal(reviewprep.Mapping{
			Schema: reviewprep.MapSchema, ID: id, BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigestOf(ref),
			GuestEntrypoint: "/bin/busybox", EntrypointSHA256: digest, Argv: argv, EgressPolicyID: policy,
		})
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "review", "maps"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "review", "maps", id+".json"), raw, 0o600))
	}
	prepare := func(id string) controlproto.ReviewRunArgs {
		bundle := t.TempDir()
		ev := []byte(`{"id":"` + id + `"}`)
		require.NoError(t, os.WriteFile(filepath.Join(bundle, "evidence.json"), ev, 0o600))
		h := sha256.Sum256(ev)
		nonce := "nonce-" + id + "-0123456789abcdef"
		raw, err := svc.PrepareReview(ctx, controlproto.ReviewPrepareArgs{
			Profile: reviewprep.Profile, Image: ref, MappingID: id, BindingDigest: strings.Repeat("b", 64),
			BundleDir: bundle, BundleDigest: hex.EncodeToString(h[:]), Nonce: nonce,
		})
		require.NoError(t, err)
		var att reviewprep.Attestation
		require.NoError(t, json.Unmarshal(raw, &att))
		return controlproto.ReviewRunArgs{
			Profile: reviewprep.Profile, Image: ref, MappingID: id, BindingDigest: strings.Repeat("b", 64),
			BundleDigest: hex.EncodeToString(h[:]), Nonce: nonce, Lease: att.Lease, MaxSeconds: 120,
		}
	}
	// The reviewer is one exec (the launcher refuses fork), so it fetches with
	// busybox wget to stdout: no shell, no pipe, no file written.
	fetchFrom := func(ip netip.Addr, port string) []string {
		return []string{"wget", "-q", "-T", "8", "-O", "-", "http://" + ip.String() + ":" + port + "/"}
	}
	fetch := func(port string) []string { return fetchFrom(allowedTestIP, port) }
	gone := func(id string) {
		t.Helper()
		assert.False(t, runner.Running(id), "the egress stack outlived the review guest %s", id)
		_, err := os.Stat("/sys/class/net/" + egress.TapName(id))
		assert.True(t, os.IsNotExist(err), "the review guest's tap %s outlived it", egress.TapName(id))
	}

	// 1. The granted destination is reached, with the destination's bytes.
	install("egress-ok", "review-net", fetch("443")...)
	raw, err := svc.RunReview(ctx, prepare("egress-ok"))
	require.NoError(t, err)
	var res controlproto.ReviewResult
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.Equal(t, 0, res.ExitCode, string(res.Stderr))
	assert.Contains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections(), "the host saw no flow from the reviewer to the granted destination")
	id, hadGrant := bind.last()
	assert.True(t, hadGrant, "a mapping that names an egress policy must hand the binder a grant function")
	gone(id)

	// 2. A destination that was not granted is not reached: the same host, a
	// port off the grant. The stand-in sees no new flow.
	install("egress-other-port", "review-net", fetch("444")...)
	raw, err = svc.RunReview(ctx, prepare("egress-other-port"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.NotZero(t, res.ExitCode, "an ungranted destination answered: %s", res.Stdout)
	assert.NotContains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections(), "a flow to an ungranted port reached the destination")
	id, _ = bind.last()
	gone(id)

	// 2b. Another address on the granted port is not reached either: the grant
	// names one exact address, not a port. The address is public (a private or
	// documentation range would be refused before any grant is consulted), and
	// the stand-in would answer it, so only the grant stands in the way.
	offGrant := netip.MustParseAddr("140.82.112.4")
	require.NotEqual(t, allowedTestIP, offGrant)
	install("egress-other-address", "review-net", fetchFrom(offGrant, "443")...)
	raw, err = svc.RunReview(ctx, prepare("egress-other-address"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.NotZero(t, res.ExitCode, "an ungranted address answered: %s", res.Stdout)
	assert.NotContains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections(), "a flow to an ungranted address on the granted port reached the destination")
	id, _ = bind.last()
	gone(id)

	// 3. A mapping with no egress policy: no grant function, no network.
	install("egress-none", "none", fetch("443")...)
	raw, err = svc.RunReview(ctx, prepare("egress-none"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.NotZero(t, res.ExitCode, "a guest with no network reached a destination: %s", res.Stdout)
	assert.Equal(t, 1, connections(), "a guest with no network reached the destination")
	_, hadGrant = bind.last()
	assert.False(t, hadGrant, "a mapping with no egress policy must not hand the binder a grant function")

	// 4. A grant the host does not admit (a name, not an ip:port) refuses the
	// review before the reviewer starts, and still takes the stack down.
	bind.entry = "allowed.test:443"
	install("egress-bad-grant", "review-net", fetch("443")...)
	raw, err = svc.RunReview(ctx, prepare("egress-bad-grant"))
	var e *reviewprep.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, reviewprep.CodePolicyUnenforcable, e.Code)
	assert.False(t, e.ReviewerStarted)
	assert.Nil(t, raw)
	id, _ = bind.last()
	gone(id)

	// Every review guest was removed.
	infos, err := mgr.List(ctx)
	require.NoError(t, err)
	for _, in := range infos {
		assert.NotContains(t, in.TaskID, "REVIEW-", "a review guest outlived its run: %s", in.ID)
	}
}
