package libkrun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/egress"
)

// notAMissingBackend fails the test when err claims the backend is missing:
// the libkrun backend is linked and running in every case below, and "no
// sandbox backend available on this platform" would send the reader to
// install a hypervisor that is already there. Refs: MGIT-232.1, MGIT-232
func notAMissingBackend(t *testing.T, err error, mustName string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if errors.Is(err, model.ErrSandboxBackendUnavailable) || strings.Contains(err.Error(), "no sandbox backend available") {
		t.Errorf("the backend is present; the refusal must not say it is missing: %v", err)
	}
	if !strings.Contains(err.Error(), mustName) {
		t.Errorf("the refusal must still name %q: %v", mustName, err)
	}
}

// A published guest port outside 1..65535 is an invalid request, named as
// that (a ValidationError on publish_port), wherever a port is turned into a
// vsock port or dialed through the gateway. Refs: MGIT-232.1
func TestPublishedPortOutOfRange_IsAnInvalidRequestNotAMissingBackend(t *testing.T) {
	for _, port := range []int{0, 70000} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			_, err := publishVsockPort(port)
			notAMissingBackend(t, err, strconv.Itoa(port))
			var v *model.ValidationError
			if !errors.As(err, &v) || v.Field != "publish_port" {
				t.Errorf("want a ValidationError on publish_port, got %v", err)
			}

			gw, gerr := bindNetGateway(filepath.Join(shortTempDir(t), proxySocketName), netDeps{auth: &stubAuthorizer{}})
			if gerr != nil {
				t.Fatalf("bindNetGateway: %v", gerr)
			}
			t.Cleanup(func() { _ = gw.Close() })
			_, err = gw.DialGuestPort(context.Background(), port)
			notAMissingBackend(t, err, strconv.Itoa(port))
			if !errors.As(err, &v) || v.Field != "publish_port" {
				t.Errorf("want a ValidationError on publish_port from the gateway, got %v", err)
			}
		})
	}
}

// The VM control channel failing to clear, bind or restrict its socket is a
// host I/O fault on a backend that is present, and is said as that, with the
// path and the I/O error. Refs: MGIT-232.1
func TestControlChannelSocketIO_IsAnIOFaultNotAMissingBackend(t *testing.T) {
	notADir := filepath.Join(shortTempDir(t), "state-is-a-file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stop, err := serveControlChannel(vmSpec{StateDir: notADir, SandboxID: "01JSB", TaskID: "T-1"},
		&egress.Supervisor{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if stop != nil {
		stop()
	}
	notAMissingBackend(t, err, notADir)
}
