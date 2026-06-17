// Package execwire is the single source of the host<->guest exec wire
// protocol (FR-17.11). One connection carries one exec: a length-prefixed
// JSON ExecRequest from the host to the guest, then a stream of typed
// frames back — stdout ('O') and stderr ('E') as the child writes them,
// then a terminal result ('R') carrying the exit code and resource usage.
//
// Both ends import this package — the guest supervisor (internal/guest)
// writes frames and the host exec client reads them — so the framing
// cannot drift between the two: a divergence in a frame byte or a length
// encoding would silently corrupt or hang the channel rather than fail to
// compile. The guest is the hostile party, so every read here enforces a
// ceiling before allocating. The land protocol is a separate IDD
// (MGIT-11.8.2); this is the exec channel only. Refs: FR-17.11
package execwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// Frame kinds carried in the 1-byte frame header.
const (
	FrameStdout = 'O' // a chunk of the child's stdout
	FrameStderr = 'E' // a chunk of the child's stderr
	FrameResult = 'R' // the terminal result frame (ResultFrame JSON)

	// MaxRequestBytes caps an inbound exec request (argv + env). The guest
	// peer is untrusted, so the reader enforces this before allocating.
	MaxRequestBytes = 1 << 20 // 1 MiB

	// MaxFrameBytes caps a single response frame the host reads from the
	// guest. Child writes stream in small chunks, so this is a generous
	// ceiling that still denies a hostile guest an unbounded allocation.
	MaxFrameBytes = 16 << 20 // 16 MiB

	frameHeaderLen = 5 // 1 type byte + 4-byte big-endian length
)

// ResourceUsage is the child's CPU usage, reported host-ward.
type ResourceUsage struct {
	UserTime   time.Duration `json:"user_time_ns"`
	SystemTime time.Duration `json:"system_time_ns"`
}

// Result is one exec's outcome: exit code plus resource usage. Stdout and
// stderr are streamed as frames during the run, not carried here.
type Result struct {
	ExitCode int           `json:"exit_code"`
	Usage    ResourceUsage `json:"usage"`
}

// ResultFrame is the JSON payload of the terminal result frame: the
// outcome plus an optional error string. A non-empty Error means the
// guest could not start the child (a supervisor-level failure, distinct
// from a non-zero exit, which is a normal Result).
type ResultFrame struct {
	Result Result `json:"outcome"`
	Error  string `json:"error,omitempty"`
}

// WriteRequest writes one length-prefixed JSON ExecRequest (host -> guest).
func WriteRequest(w io.Writer, req model.ExecRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("execwire: encode request: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // request size is bounded by the caller and the guest cap
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("execwire: write request length: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("execwire: write request: %w", err)
	}
	return nil
}

// ReadRequest reads one length-prefixed JSON ExecRequest, enforcing the
// MaxRequestBytes ceiling before allocating. Refs: FR-17.11
func ReadRequest(r io.Reader) (model.ExecRequest, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return model.ExecRequest{}, fmt.Errorf("execwire: read request length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxRequestBytes {
		return model.ExecRequest{}, fmt.Errorf("execwire: exec request %d bytes exceeds %d cap", n, MaxRequestBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return model.ExecRequest{}, fmt.Errorf("execwire: read request: %w", err)
	}
	var req model.ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return model.ExecRequest{}, fmt.Errorf("execwire: parse request: %w", err)
	}
	return req, nil
}

// WriteFrame writes one typed, length-prefixed frame (guest -> host).
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	var hdr [frameHeaderLen]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload))) //nolint:gosec // frame payloads are bounded by MaxFrameBytes on read
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

// ReadFrame reads one typed, length-prefixed frame, enforcing the
// MaxFrameBytes ceiling (a hostile guest must not be able to demand an
// unbounded host allocation). A clean io.EOF read before any header byte
// is returned verbatim so callers can detect end-of-stream.
func ReadFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrameBytes {
		return 0, nil, fmt.Errorf("execwire: response frame %d bytes exceeds %d cap", n, MaxFrameBytes)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("execwire: read frame body: %w", err)
	}
	return hdr[0], payload, nil
}
