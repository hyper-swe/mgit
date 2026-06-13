package guest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/hyper-swe/mgit/internal/model"
)

// Exec wire framing. One connection carries one exec request: a
// length-prefixed JSON ExecRequest in, then a stream of typed frames
// out (stdout/stderr as the child writes, a final result frame). The
// land protocol and its hardened schema/ceilings are a separate IDD
// (MGIT-11.8.2); this is the exec channel only.
const (
	frameStdout = 'O'
	frameStderr = 'E'
	frameResult = 'R'

	// maxExecRequestBytes caps an inbound exec request (argv + env);
	// the daemon also caps at vsock framing (MGIT-11.4.1). A guest
	// peer is untrusted, so the supervisor enforces its own ceiling.
	maxExecRequestBytes = 1 << 20 // 1 MiB
)

// resultFrame is the JSON payload of the terminal result frame.
type resultFrame struct {
	Outcome Outcome `json:"outcome"`
	Error   string  `json:"error,omitempty"`
}

// Serve handles one exec connection: decode the request, stream the
// child's stdout/stderr as frames, and send a terminal result frame
// carrying the exit code and resource usage (reported to the host).
// Refs: FR-17.11
func (s *Supervisor) Serve(ctx context.Context, conn io.ReadWriter) error {
	req, err := readRequest(conn)
	if err != nil {
		return s.sendResult(conn, Outcome{}, err)
	}

	// One mutex serializes stdout/stderr frames on the shared conn.
	var mu sync.Mutex
	stdout := &frameWriter{mu: &mu, w: conn, kind: frameStdout}
	stderr := &frameWriter{mu: &mu, w: conn, kind: frameStderr}

	outcome, execErr := s.Execute(ctx, req, stdout, stderr)
	if s.Logger != nil {
		s.Logger.Info("guest exec served", "event", "exec",
			"exit_code", outcome.ExitCode,
			"user_time_ns", outcome.Usage.UserTime,
			"system_time_ns", outcome.Usage.SystemTime)
	}
	return s.sendResult(conn, outcome, execErr)
}

// sendResult writes the terminal result frame.
func (s *Supervisor) sendResult(w io.Writer, outcome Outcome, execErr error) error {
	frame := resultFrame{Outcome: outcome}
	if execErr != nil {
		frame.Error = execErr.Error()
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("guest serve: encode result: %w", err)
	}
	if err := writeFrame(w, frameResult, payload); err != nil {
		return fmt.Errorf("guest serve: write result: %w", err)
	}
	return execErr
}

// readRequest reads one length-prefixed JSON ExecRequest, capped.
func readRequest(r io.Reader) (model.ExecRequest, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return model.ExecRequest{}, fmt.Errorf("guest serve: read request length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxExecRequestBytes {
		return model.ExecRequest{}, fmt.Errorf("guest serve: exec request %d bytes exceeds %d cap", n, maxExecRequestBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return model.ExecRequest{}, fmt.Errorf("guest serve: read request: %w", err)
	}
	var req model.ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return model.ExecRequest{}, fmt.Errorf("guest serve: parse request: %w", err)
	}
	return req, nil
}

// writeFrame writes one typed, length-prefixed frame.
func writeFrame(w io.Writer, kind byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload))) //nolint:gosec // frame payloads are bounded
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// frameWriter emits each Write as one frame of a fixed kind, so the
// child's stdout/stderr stream to the host as it is produced. The
// shared mutex makes each header+payload pair atomic, so stdout and
// stderr frames never interleave on the connection.
type frameWriter struct {
	mu   *sync.Mutex
	w    io.Writer
	kind byte
}

// Write emits p as one frame.
func (f *frameWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := writeFrame(f.w, f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
