package guestexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/guest"
	"github.com/hyper-swe/mgit/internal/model"
)

// legacyResultFrame is the result frame exactly as a daemon built before the
// version handshake declared it, frozen here. It is decoded with a plain
// json.Unmarshal, which is what that daemon did.
type legacyResultFrame struct {
	Result struct {
		ExitCode int `json:"exit_code"`
	} `json:"outcome"`
	Error string `json:"error,omitempty"`
}

// An older daemon in front of a new guest keeps working: the REAL guest's
// result frame, with its protocol and build fields, decodes under the frozen
// legacy shape with the error and exit code intact.
// Refs: MGIT-158
func TestOldDaemon_DecodesTheRealGuestsFrame(t *testing.T) {
	var in bytes.Buffer
	require.NoError(t, execwire.WriteRequest(&in,
		model.ExecRequest{Command: []string{"/nonexistent/mgit-guest-readiness-probe"}}))
	var out bytes.Buffer
	sup := guest.NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = sup.Serve(context.Background(), struct {
		io.Reader
		io.Writer
	}{&in, &out})
	kind, payload, err := execwire.ReadFrame(&out)
	require.NoError(t, err)
	require.Equal(t, byte(execwire.FrameResult), kind)
	require.Contains(t, string(payload), "guest_protocol", "the new guest does send the new fields")

	var legacy legacyResultFrame
	require.NoError(t, json.Unmarshal(payload, &legacy), "a daemon that does not know the new fields must still decode the frame")
	assert.NotEmpty(t, legacy.Error, "the refusal text survives")
}

// The shipping decoder also ignores a field it has never heard of, so a guest
// one protocol-compatible step ahead cannot break it.
func TestRun_IgnoresFieldsItDoesNotKnow(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := execwire.ReadRequest(server); err != nil {
			return
		}
		_ = execwire.WriteFrame(server, execwire.FrameResult,
			[]byte(`{"outcome":{"exit_code":3,"from_the_future":true},"also_new":"x"}`))
	}()
	res, err := Run(client, model.ExecRequest{Command: []string{"/bin/true"}}, io.Discard, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 3, res.ExitCode)
}

// Nothing that reads a peer's frame may turn on strict decoding: that is the
// one change that would make every older or newer peer fail at once.
func TestWireDecoders_NeverRejectUnknownFields(t *testing.T) {
	for _, dir := range []string{"../../execwire", "."} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // repo source
			require.NoError(t, err)
			assert.NotContains(t, string(b), "DisallowUnknownFields", "%s/%s", dir, e.Name())
		}
	}
}
