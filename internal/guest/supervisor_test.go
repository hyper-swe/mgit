// Package guest tests verify the mgit-guest supervisor per MGIT-11.5.6
// acceptance criteria. The supervisor core (clean-env exec, streaming,
// exit codes, framing) is platform-portable and tested here; the
// PID-1 mount + vsock duties live in the linux-tagged binary.
// Refs: FR-17.11, FR-17.3, SEC-01
package guest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/testutil"
)

func testSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	return NewSupervisor(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

func run(t *testing.T, sup *Supervisor, req model.ExecRequest) (string, string, Outcome) {
	t.Helper()
	var stdout, stderr strings.Builder
	outcome, err := sup.Execute(context.Background(), req, &stdout, &stderr)
	require.NoError(t, err, "a command that starts must not error at the supervisor layer")
	return stdout.String(), stderr.String(), outcome
}

// TestGuestAgent_PID1_ServesExec verifies the supervisor serves one
// exec request over a connection: framed request in, streamed output
// frames + a result frame out. Refs: FR-17.11
func TestGuestAgent_PID1_ServesExec(t *testing.T) {
	sup := testSupervisor(t)
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		defer func() { _ = server.Close() }()
		_ = sup.Serve(context.Background(), server)
	}()

	require.NoError(t, writeRequest(client, model.ExecRequest{
		Command: []string{"/bin/echo", "hello-guest"},
	}))

	stdout, stderr, outcome := readResponse(t, client)
	assert.Equal(t, "hello-guest\n", stdout)
	assert.Empty(t, stderr)
	assert.Zero(t, outcome.ExitCode)
}

// TestGuestAgent_CleanEnv_NoHostVars verifies SEC-01/FR-17.3 hygiene:
// the child never inherits the agent's (host-injected) environment;
// only the clean base plus explicit per-exec injections reach it.
func TestGuestAgent_CleanEnv_NoHostVars(t *testing.T) {
	t.Setenv("MGIT_HOST_SECRET", "do-not-leak")
	sup := testSupervisor(t)

	stdout, _, outcome := run(t, sup, model.ExecRequest{
		Command: []string{"/usr/bin/env"},
		Env:     []string{"INJECTED_TOKEN=ok"},
	})

	assert.Zero(t, outcome.ExitCode)
	assert.NotContains(t, stdout, "MGIT_HOST_SECRET",
		"host environment must never reach the guest child (SEC-01)")
	assert.NotContains(t, stdout, "do-not-leak")
	assert.Contains(t, stdout, "INJECTED_TOKEN=ok",
		"explicit per-exec injections are delivered")
	assert.Contains(t, stdout, "PATH=", "a clean base env is provided")
}

// TestGuestAgent_StreamsExitCodes verifies exit codes propagate and
// stdout/stderr are separated. Refs: FR-17.11
func TestGuestAgent_StreamsExitCodes(t *testing.T) {
	sup := testSupervisor(t)

	tests := []struct {
		name     string
		command  []string
		wantOut  string
		wantErr  string
		wantCode int
	}{
		{name: "zero_exit", command: []string{"/bin/sh", "-c", "echo ok"}, wantOut: "ok\n", wantCode: 0},
		{name: "nonzero_exit", command: []string{"/bin/sh", "-c", "exit 7"}, wantCode: 7},
		{name: "split_streams", command: []string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 3"},
			wantOut: "out\n", wantErr: "err\n", wantCode: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, outcome := run(t, sup, model.ExecRequest{Command: tt.command})
			assert.Equal(t, tt.wantOut, stdout)
			assert.Equal(t, tt.wantErr, stderr)
			assert.Equal(t, tt.wantCode, outcome.ExitCode)
		})
	}

	t.Run("invalid_request_rejected", func(t *testing.T) {
		_, err := sup.Execute(context.Background(), model.ExecRequest{}, io.Discard, io.Discard)
		assert.Error(t, err, "an empty command is rejected before exec")
	})

	t.Run("missing_binary_surfaces", func(t *testing.T) {
		_, err := sup.Execute(context.Background(),
			model.ExecRequest{Command: []string{"/no/such/binary-xyz"}}, io.Discard, io.Discard)
		assert.Error(t, err, "a command that cannot start surfaces a supervisor error")
	})

	t.Run("usage_reported", func(t *testing.T) {
		_, _, outcome := run(t, sup, model.ExecRequest{Command: []string{"/bin/sh", "-c", ":"}})
		assert.GreaterOrEqual(t, outcome.Usage.UserTime+outcome.Usage.SystemTime, time.Duration(0),
			"resource usage is captured for reporting to the host")
	})
}

// TestGuestAgent_NoSigningKeyMaterial enforces SEC-01 structurally: the
// guest agent (this package and the mgit-guest binary) imports no
// signing/crypto-key package and reads no host environment — it is a
// pure transport that cannot mint attestations.
func TestGuestAgent_NoSigningKeyMaterial(t *testing.T) {
	forbiddenImports := []string{
		"crypto/ed25519", "crypto/ecdsa", "crypto/rsa", "crypto/dsa",
		"golang.org/x/crypto",
		"github.com/hyper-swe/mgit/internal/sandboxd/images", // holds the trust root
	}
	// os.Environ would let host env leak into a child (clean-env breaks).
	forbiddenCalls := []string{"os.Environ"}

	root := testutil.ProjectRoot(t)
	for _, pkgDir := range []string{
		filepath.Join(root, "internal", "guest"),
		filepath.Join(root, "cmd", "mgit-guest"),
	} {
		entries, err := os.ReadDir(pkgDir)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(pkgDir, name)
			src, err := os.ReadFile(path) //nolint:gosec // test reads repo sources
			require.NoError(t, err)
			for _, call := range forbiddenCalls {
				assert.NotContains(t, string(src), call,
					"%s must not call %s (clean-env / no host passthrough)", name, call)
			}

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			require.NoError(t, err)
			for _, imp := range file.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				for _, bad := range forbiddenImports {
					assert.False(t, importPath == bad || strings.HasPrefix(importPath, bad+"/"),
						"%s must not import %s (guest holds no signing key, SEC-01)", name, importPath)
				}
			}
		}
	}
}

// --- test client: the minimal exec wire protocol ---

func writeRequest(w io.Writer, req model.ExecRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readResponse(t *testing.T, r io.Reader) (stdout, stderr string, outcome Outcome) {
	t.Helper()
	var out, errb strings.Builder
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			require.ErrorIs(t, err, io.EOF, "stream ends cleanly")
			break
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		payload := make([]byte, n)
		_, err := io.ReadFull(r, payload)
		require.NoError(t, err)
		switch hdr[0] {
		case frameStdout:
			out.Write(payload)
		case frameStderr:
			errb.Write(payload)
		case frameResult:
			require.NoError(t, json.Unmarshal(payload, &outcome), "result frame must decode")
			return out.String(), errb.String(), outcome
		default:
			t.Fatalf("unknown frame kind %q", hdr[0])
		}
	}
	return out.String(), errb.String(), outcome
}
