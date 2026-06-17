package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/hyper-swe/mgit/internal/execwire"
)

// Serve handles one exec connection: decode the request, stream the
// child's stdout/stderr as frames, and send a terminal result frame
// carrying the exit code and resource usage (reported to the host). The
// wire protocol is defined once in internal/execwire. Refs: FR-17.11
func (s *Supervisor) Serve(ctx context.Context, conn io.ReadWriter) error {
	req, err := execwire.ReadRequest(conn)
	if err != nil {
		return s.sendResult(conn, Outcome{}, err)
	}

	// One mutex serializes stdout/stderr frames on the shared conn.
	var mu sync.Mutex
	stdout := &frameWriter{mu: &mu, w: conn, kind: execwire.FrameStdout}
	stderr := &frameWriter{mu: &mu, w: conn, kind: execwire.FrameStderr}

	outcome, execErr := s.Execute(ctx, req, stdout, stderr)
	if s.Logger != nil {
		s.Logger.Info("guest exec served", "event", "exec",
			"exit_code", outcome.ExitCode,
			"user_time_ns", outcome.Usage.UserTime,
			"system_time_ns", outcome.Usage.SystemTime)
	}
	return s.sendResult(conn, outcome, execErr)
}

// sendResult writes the terminal result frame and returns execErr so a
// guest-side start failure both reaches the host and surfaces locally.
func (s *Supervisor) sendResult(w io.Writer, outcome Outcome, execErr error) error {
	frame := execwire.ResultFrame{Result: outcome}
	if execErr != nil {
		frame.Error = execErr.Error()
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("guest serve: encode result: %w", err)
	}
	if err := execwire.WriteFrame(w, execwire.FrameResult, payload); err != nil {
		return fmt.Errorf("guest serve: write result: %w", err)
	}
	return execErr
}

// frameWriter emits each Write as one frame of a fixed kind, so the
// child's stdout/stderr stream to the host as they are produced. The
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
	if err := execwire.WriteFrame(f.w, f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
