//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewbind"
	"github.com/hyper-swe/mgit/internal/reviewprep"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/attest"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// TestE2E_ReviewBind_GrantsTheResolvedAddressOn443AndNothingElse drives the
// real binding (reviewbind.New) through a live review guest: the policy host
// resolves to one address, and the guest reaches that address on 443 only. An
// address off the grant, and a port off the grant, are not reached.
func TestE2E_ReviewBind_GrantsTheResolvedAddressOn443AndNothingElse(t *testing.T) {
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

	target, connections := httpStandIn(t)
	runner, err := egress.NewRunner(egress.RunnerConfig{
		Audit:  &recordingEgressAudit{},
		Lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, egress.ErrNXDOMAIN },
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
	// The policy's host resolves to allowedTestIP; nothing else is granted.
	bind := reviewbind.New(reviewbind.Config{
		Lookup: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{allowedTestIP}, nil },
	})
	svc.WithRunner(reviewsvc.RunnerConfig{
		Manager: mgr, Backend: model.BackendKVM, Bindings: bind,
		Egress: reviewEgressStack{EgressController: NewEgressController(runner), Runner: runner},
	})

	install := func(id string, argv ...string) {
		raw, _ := json.Marshal(reviewprep.Mapping{
			Schema: reviewprep.MapSchema, ID: id, BindingSHA256: strings.Repeat("b", 64), ImageDigest: imageDigestOf(ref),
			GuestEntrypoint: "/bin/busybox", EntrypointSHA256: digest, Argv: argv, EgressPolicyID: "host:review.example.com",
		})
		require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, "review", "maps"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "review", "maps", id+".json"), raw, 0o600))
	}
	run := func(id string) controlproto.ReviewResult {
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
		out, err := svc.RunReview(ctx, controlproto.ReviewRunArgs{
			Profile: reviewprep.Profile, Image: ref, MappingID: id, BindingDigest: strings.Repeat("b", 64),
			BundleDigest: hex.EncodeToString(h[:]), Nonce: nonce, Lease: att.Lease, MaxSeconds: 120,
		})
		require.NoError(t, err)
		var res controlproto.ReviewResult
		require.NoError(t, json.Unmarshal(out, &res))
		return res
	}
	fetch := func(addr, port string) []string {
		return []string{"wget", "-q", "-T", "8", "-O", "-", "http://" + addr + ":" + port + "/"}
	}

	// 1. The resolved address on 443 is reached, with the destination's bytes.
	install("bind-ok", fetch(allowedTestIP.String(), "443")...)
	res := run("bind-ok")
	assert.Equal(t, 0, res.ExitCode, string(res.Stderr))
	assert.Contains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections())

	// 2. An address off the grant, on the granted port, is not reached.
	offGrant := netip.MustParseAddr("198.51.100.77")
	require.NotEqual(t, allowedTestIP, offGrant)
	install("bind-off-address", fetch(offGrant.String(), "443")...)
	res = run("bind-off-address")
	assert.NotZero(t, res.ExitCode, "an address off the grant answered: %s", res.Stdout)
	assert.NotContains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections(), "a flow to an address off the grant reached the destination")

	// 3. The granted address on another port is not reached.
	install("bind-off-port", fetch(allowedTestIP.String(), "8443")...)
	res = run("bind-off-port")
	assert.NotZero(t, res.ExitCode, "a port off the grant answered: %s", res.Stdout)
	assert.NotContains(t, string(res.Stdout), reviewEgressBody)
	assert.Equal(t, 1, connections(), "a flow to a port off the grant reached the destination")
}
