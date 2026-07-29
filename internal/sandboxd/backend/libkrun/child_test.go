package libkrun

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// The child process is the ONLY place a libkrun VM boots (krun_start_enter
// seizes and exit()s its process — ADR-010), so its sequence is tested here
// end to end with a fake krunAPI: spec in, policy assembled per mode, context
// configured through the newGuestCtx funnel, handshake protocol honored, and
// everything released on failure. Refs: ADR-010, MGIT-61.8

func testChildLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
}

// decodeHandshakes parses every handshake line the child wrote.
func decodeHandshakes(t *testing.T, buf *bytes.Buffer) []childHandshake {
	t.Helper()
	var out []childHandshake
	sc := bufio.NewScanner(buf)
	for sc.Scan() {
		var h childHandshake
		if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
			t.Fatalf("handshake line %q: %v", sc.Text(), err)
		}
		out = append(out, h)
	}
	return out
}

func TestChildRun_NoneMode_ConfiguresEntersAndReportsBootFailure(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{}
	var handshake bytes.Buffer

	// The fake's StartEnter always fails: a fake cannot model "never
	// returns", so this test exercises configure -> ok -> enter -> pre-boot
	// failure -> second handshake line.
	code := childRun(api, baseSpec(model.NetworkModeNone, dir), &handshake, testChildLogger(), testClock())

	if code == 0 {
		t.Error("a childRun that RETURNS is always a failure (success never returns), want non-zero exit")
	}
	hs := decodeHandshakes(t, &handshake)
	if len(hs) != 2 || !hs[0].OK || hs[1].OK || hs[1].Error == "" {
		t.Fatalf("handshakes = %+v, want [configured-ok, boot-failure-with-error]", hs)
	}
	if !strings.HasSuffix(api.seq(), "start_enter,free_ctx") {
		t.Errorf("sequence %q: the VM must be entered after the ok handshake and freed on failure", api.seq())
	}
	if denySocketExists(dir) {
		t.Error("deny socket left bound after the child failed (SEC-10 residue)")
	}
}

func TestChildRun_ConfigurationFails_ReportsBeforeEnteringAndNeverSaysOK(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{failOn: "set_vm_config"}
	var handshake bytes.Buffer

	code := childRun(api, baseSpec(model.NetworkModeNone, dir), &handshake, testChildLogger(), testClock())

	if code == 0 {
		t.Error("want non-zero exit on configuration failure")
	}
	hs := decodeHandshakes(t, &handshake)
	if len(hs) != 1 || hs[0].OK || hs[0].Error == "" {
		t.Fatalf("handshakes = %+v, want exactly one failure line (never ok)", hs)
	}
	if strings.Contains(api.seq(), "start_enter") {
		t.Errorf("sequence %q: a failed configuration must never enter the VM", api.seq())
	}
}

func TestChildRun_AllowlistMode_AssemblesPolicyAndCleansUpTheGateway(t *testing.T) {
	dir := shortTempDir(t)
	api := &fakeKrun{}
	var handshake bytes.Buffer

	spec := baseSpec(model.NetworkModeAllowlist, dir)
	spec.TaskID = "MGIT-61.8"
	spec.Allowlist = []string{"proxy.golang.org:443"}

	code := childRun(api, spec, &handshake, testChildLogger(), testClock())

	if code == 0 {
		t.Error("want non-zero exit (fake StartEnter fails)")
	}
	// The assembly built and the context configured: the ok line proves the
	// full allowlist path (compile + resolver + authorizer + DNS + gateway).
	hs := decodeHandshakes(t, &handshake)
	if len(hs) != 2 || !hs[0].OK {
		t.Fatalf("handshakes = %+v, want configured-ok first: allowlist mode must be SERVED", hs)
	}
	if _, err := os.Stat(filepath.Join(dir, proxySocketName)); !os.IsNotExist(err) {
		t.Error("gateway socket left bound after the child failed (SEC-10 residue)")
	}
}

func TestChildPolicy_PerMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantAuth bool
		wantDNS  bool
	}{
		// none runs the discard socket and needs no policy; allowlist and open
		// both run the gateway, so both must carry an authorizer.
		{name: "none_has_no_policy", mode: model.NetworkModeNone},
		{name: "allowlist_gets_authorizer_and_dns", mode: model.NetworkModeAllowlist, wantAuth: true, wantDNS: true},
		// Open is unrestricted but still AUDITED, so it gets an authorizer —
		// and no DNS resolver, because it connects by address.
		{name: "open_gets_an_allow_all_authorizer", mode: model.NetworkModeOpen, wantAuth: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := baseSpec(tt.mode, shortTempDir(t))
			auth, dns, err := childPolicy(spec, testChildLogger(), testClock())
			if err != nil {
				t.Fatalf("childPolicy: %v", err)
			}
			if (auth != nil) != tt.wantAuth {
				t.Errorf("authorizer present = %v, want %v", auth != nil, tt.wantAuth)
			}
			if (dns != nil) != tt.wantDNS {
				t.Errorf("dns present = %v, want %v", dns != nil, tt.wantDNS)
			}
		})
	}
}

func TestChildMain_RejectsUnsupportedSpecsBeforeBooting(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		want  string
	}{
		{name: "malformed_json", stdin: "not json", want: "decode"},
		// The child RE-VALIDATES rather than trusting the parent: a spec the
		// backend cannot act on is refused on its own side too.
		{name: "empty_spec", stdin: `{"sandbox_id":""}`, want: "sandbox id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr, handshake bytes.Buffer
			// childMain, not ChildMain: the exported wrapper wraps REAL fd 3,
			// which in this (non-spawned) process belongs to the Go runtime.
			code := childMain(strings.NewReader(tt.stdin), &handshake, &stderr)
			if code == 0 {
				t.Error("want non-zero exit")
			}
			if !strings.Contains(stderr.String(), "krun_vm_failed") {
				t.Errorf("stderr %q lacks the failure log event", stderr.String())
			}
			hs := decodeHandshakes(t, &handshake)
			if len(hs) != 1 || hs[0].OK || !strings.Contains(hs[0].Error, tt.want) {
				t.Fatalf("handshakes = %+v, want one failure mentioning %q", hs, tt.want)
			}
		})
	}
}

func TestWriteHandshake_NilPipe_DoesNotPanic(t *testing.T) {
	// A hand-run child has no fd-3 pipe; reporting must degrade, not crash.
	writeHandshake(nil, testChildLogger(), childHandshake{OK: true})
}
