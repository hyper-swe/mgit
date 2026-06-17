package guestexec

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/guest"
	"github.com/hyper-swe/mgit/internal/model"
)

// skipWithoutPOSIXShell skips tests that exec POSIX commands. The guest
// runs only inside the Linux microVM (FR-17.16); the supervisor's exec
// path is exercised on Linux/darwin runners, not Windows.
func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec routes POSIX commands; the guest runs only inside the Linux microVM (FR-17.16)")
	}
}

// runViaGuest routes req through guestexec.Run to a real guest.Supervisor
// over an in-memory pipe — the same wire protocol as production vsock,
// without a VM. It returns the streamed stdout/stderr and the result.
func runViaGuest(t *testing.T, req model.ExecRequest) (stdout, stderr string, res execwire.Result, err error) {
	t.Helper()
	sup := guest.NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_ = sup.Serve(context.Background(), server)
	}()
	defer func() { _ = client.Close() }()

	var out, errb bytes.Buffer
	res, err = Run(client, req, &out, &errb)
	return out.String(), errb.String(), res, err
}

// TestExec_ExitCodePropagated verifies the guest's exit code reaches the
// host unchanged — both zero and non-zero, the latter without an error.
// Refs: FR-17.11
func TestExec_ExitCodePropagated(t *testing.T) {
	skipWithoutPOSIXShell(t)
	t.Run("zero", func(t *testing.T) {
		out, _, res, err := runViaGuest(t, model.ExecRequest{Command: []string{"/bin/sh", "-c", "echo ok"}})
		require.NoError(t, err)
		assert.Equal(t, 0, res.ExitCode)
		assert.Equal(t, "ok\n", out)
	})
	t.Run("nonzero_is_not_an_error", func(t *testing.T) {
		_, _, res, err := runViaGuest(t, model.ExecRequest{Command: []string{"/bin/sh", "-c", "exit 7"}})
		require.NoError(t, err, "a non-zero exit is a normal result, not a transport error")
		assert.Equal(t, 7, res.ExitCode)
	})
}

// TestExec_PipelineBehavesAsLocal verifies a whole-command shell pipeline
// runs as one guest shell, so pipes/globs behave as they would locally.
// Refs: FR-17.11
func TestExec_PipelineBehavesAsLocal(t *testing.T) {
	skipWithoutPOSIXShell(t)
	out, _, res, err := runViaGuest(t, model.ExecRequest{
		Command: []string{"/bin/sh", "-c", "echo hello | tr a-z A-Z"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "HELLO\n", out)
}

// TestExec_CwdPreserved verifies the request's working directory is the
// child's cwd (identical-path mount in production). Refs: FR-17.3, FR-17.11
func TestExec_CwdPreserved(t *testing.T) {
	skipWithoutPOSIXShell(t)
	dir := t.TempDir()
	// pwd reports the physical path; on macOS the temp dir resolves through
	// the /var -> /private/var symlink, so compare resolved paths.
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	out, _, res, err := runViaGuest(t, model.ExecRequest{
		Command: []string{"/bin/sh", "-c", "pwd"}, Dir: dir,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, resolved, strings.TrimSpace(out))
}

// TestExec_WarmOverheadUnderTarget verifies the exec transport adds little
// round-trip overhead. It measures the part 11.9.2 owns — request framing,
// guest dispatch, streamed result — not VM boot. The minimum over a few
// warm runs filters scheduler/GC spikes. Refs: FR-17.11, NFR-17 (exec overhead)
func TestExec_WarmOverheadUnderTarget(t *testing.T) {
	skipWithoutPOSIXShell(t)
	const target = 50 * time.Millisecond
	req := model.ExecRequest{Command: []string{"/bin/sh", "-c", ":"}}
	_, _, _, err := runViaGuest(t, req) // warm
	require.NoError(t, err)

	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		_, _, _, err := runViaGuest(t, req)
		require.NoError(t, err)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	assert.Less(t, best, target, "warm exec round-trip overhead should be under %s (was %s)", target, best)
}

// --- transport error paths ----------------------------------------------

// scriptedConn serves canned response bytes and discards (or fails) writes.
type scriptedConn struct {
	resp io.Reader
	w    io.Writer
}

func (c *scriptedConn) Read(p []byte) (int, error)  { return c.resp.Read(p) }
func (c *scriptedConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// failWriter fails every write, exercising the request-send error path.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func frameBytes(t *testing.T, kind byte, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, execwire.WriteFrame(&buf, kind, payload))
	return buf.Bytes()
}

func TestRun_InvalidRequest_Rejected(t *testing.T) {
	_, err := Run(&scriptedConn{resp: bytes.NewReader(nil), w: io.Discard}, model.ExecRequest{}, io.Discard, io.Discard)
	assert.Error(t, err)
}

func TestRun_SendRequestFails(t *testing.T) {
	_, err := Run(&scriptedConn{resp: bytes.NewReader(nil), w: failWriter{}},
		model.ExecRequest{Command: []string{"x"}}, io.Discard, io.Discard)
	assert.Error(t, err)
}

func TestRun_ReadFrameEOF(t *testing.T) {
	_, err := Run(&scriptedConn{resp: bytes.NewReader(nil), w: io.Discard},
		model.ExecRequest{Command: []string{"x"}}, io.Discard, io.Discard)
	assert.Error(t, err)
}

func TestRun_UnknownFrame_Rejected(t *testing.T) {
	resp := frameBytes(t, 'X', []byte("surprise"))
	_, err := Run(&scriptedConn{resp: bytes.NewReader(resp), w: io.Discard},
		model.ExecRequest{Command: []string{"x"}}, io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown frame")
}

func TestRun_MalformedResult_Rejected(t *testing.T) {
	resp := frameBytes(t, execwire.FrameResult, []byte("{not json"))
	_, err := Run(&scriptedConn{resp: bytes.NewReader(resp), w: io.Discard},
		model.ExecRequest{Command: []string{"x"}}, io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode result")
}

func TestRun_GuestReportedError_Surfaces(t *testing.T) {
	skipWithoutPOSIXShell(t)
	// A binary that cannot start is reported by the guest as a result-frame
	// error, which Run surfaces (distinct from a clean non-zero exit).
	_, _, _, err := runViaGuest(t, model.ExecRequest{Command: []string{"/no/such/binary-xyz"}})
	assert.Error(t, err)
}

// TestRun_StreamsToWriters verifies stdout and stderr frames are written
// to their respective sinks (split streams) as they arrive.
func TestRun_StreamsToWriters(t *testing.T) {
	skipWithoutPOSIXShell(t)
	out, errb, res, err := runViaGuest(t, model.ExecRequest{
		Command: []string{"/bin/sh", "-c", "echo to-out; echo to-err 1>&2"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "to-out\n", out)
	assert.Equal(t, "to-err\n", errb)
}

// TestRun_StdoutWriteError_Surfaces verifies a failing stdout sink aborts
// the run rather than being swallowed.
func TestRun_StdoutWriteError_Surfaces(t *testing.T) {
	resp := frameBytes(t, execwire.FrameStdout, []byte("data"))
	_, err := Run(&scriptedConn{resp: bytes.NewReader(resp), w: io.Discard},
		model.ExecRequest{Command: []string{"x"}}, failWriter{}, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write stdout")
}

// TestRun_StderrWriteError_Surfaces verifies a failing stderr sink aborts.
func TestRun_StderrWriteError_Surfaces(t *testing.T) {
	resp := frameBytes(t, execwire.FrameStderr, []byte("data"))
	_, err := Run(&scriptedConn{resp: bytes.NewReader(resp), w: io.Discard},
		model.ExecRequest{Command: []string{"x"}}, io.Discard, failWriter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write stderr")
}
