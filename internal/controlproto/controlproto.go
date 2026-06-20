// Package controlproto is the single source of the host control-plane
// protocol spoken between the mgit CLI and the mgit-sandboxd daemon over
// the daemon's unix socket, AFTER the daemon's peer-UID auth + greeting.
// Both ends import it, so the request/response framing cannot drift.
//
// TRUST BOUNDARY: the socket is already same-UID only (kernel peer UID ==
// daemon UID, 0600 socket in a 0700 dir, exclusive flock). The client is
// therefore as privileged as the daemon — this protocol is ROBUSTNESS and
// defense-in-depth, NOT a privilege boundary. The real security boundary
// is host<->guest (vsock), unchanged. Accordingly every decode bounds its
// input before allocating and fails closed on malformed/oversized/unknown
// input so a buggy or hostile same-UID client can never crash, hang, or
// over-allocate the single daemon that supervises every VM.
//
// One Exec request is followed by a stream of internal/execwire frames
// (stdout 'O' / stderr 'E' / result 'R') relayed from the guest — exec
// streaming is single-sourced in execwire, not re-encoded here.
// Refs: FR-17.16, FR-17.34, MGIT-11.10.7
package controlproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// Request kinds carried in the 1-byte frame tag.
const (
	KindLaunch byte = 'L'
	KindExec   byte = 'X'
	KindLand   byte = 'D'
	KindList   byte = 'I'
	KindRemove byte = 'R'
	KindStatus byte = 'S'
)

// Ceilings enforced before allocation (the daemon supervises all VMs; a
// crafted message must never drive a large allocation or pathological
// decode). Refs: MGIT-11.10.7 (security audit)
const (
	// MaxRequestBytes caps one control request (argv + env are bounded).
	MaxRequestBytes = 1 << 20 // 1 MiB
	// MaxResponseBytes caps one control response.
	MaxResponseBytes = 1 << 20 // 1 MiB
	// MaxArgv caps an exec command's argv length.
	MaxArgv = 4096
	// MaxEnv caps the number of per-exec env injections.
	MaxEnv = 4096
	// MaxEnvEntryBytes caps one env entry's length.
	MaxEnvEntryBytes = 32 << 10 // 32 KiB
)

// DefaultRequestTimeout is the recommended per-request read deadline the
// daemon applies (SetReadDeadline) before decoding, so a slow-loris client
// cannot hold a daemon goroutine indefinitely. Exposed here so client and
// daemon agree; the deadline itself is applied on the net.Conn by the
// daemon (this package operates on plain io.Reader/Writer).
const DefaultRequestTimeout = 30 * time.Second

const frameHeaderLen = 5 // 1 kind byte + 4-byte big-endian length

// TaskRef addresses a sandbox by its bound task ID.
type TaskRef struct {
	TaskID string `json:"task_id"`
}

// RemoveArgs addresses a sandbox to tear down.
type RemoveArgs struct {
	TaskID string `json:"task_id"`
	Force  bool   `json:"force"`
}

// ExecArgs routes one command into a task's sandbox.
type ExecArgs struct {
	TaskID string            `json:"task_id"`
	Exec   model.ExecRequest `json:"exec"`
}

// Request is one control-plane request: a kind tag plus exactly the one
// payload that matches the kind (List carries none). The kind is the
// frame tag, not a JSON field.
type Request struct {
	Kind   byte                        `json:"-"`
	Launch *model.SandboxLaunchOptions `json:"launch,omitempty"`
	Exec   *ExecArgs                   `json:"exec,omitempty"`
	Land   *TaskRef                    `json:"land,omitempty"`
	Remove *RemoveArgs                 `json:"remove,omitempty"`
	Status *TaskRef                    `json:"status,omitempty"`
}

// LandResult summarizes a completed land.
type LandResult struct {
	Commits int    `json:"commits"`
	Branch  string `json:"branch"`
}

// Response is the single reply to a non-streaming request (Exec replies as
// an execwire frame stream instead). A non-empty Error means the op
// failed; the typed field for the request kind is set on success.
type Response struct {
	Error   string              `json:"error,omitempty"`
	Sandbox *model.SandboxInfo  `json:"sandbox,omitempty"` // launch, status
	List    []model.SandboxInfo `json:"list,omitempty"`    // list
	Landed  *LandResult         `json:"landed,omitempty"`  // land
}

// validKind reports whether k is a known request kind.
func validKind(k byte) bool {
	switch k {
	case KindLaunch, KindExec, KindLand, KindList, KindRemove, KindStatus:
		return true
	default:
		return false
	}
}

// WriteRequest frames and writes a request (kind tag + length-prefixed
// JSON). It refuses to emit an over-cap message.
func WriteRequest(w io.Writer, req *Request) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("controlproto: encode request: %w", err)
	}
	if len(payload) > MaxRequestBytes {
		return fmt.Errorf("controlproto: request %d bytes exceeds %d cap", len(payload), MaxRequestBytes)
	}
	return writeFrame(w, req.Kind, payload)
}

// ReadRequest reads and validates one request, enforcing the size ceiling
// before allocating, rejecting unknown kinds and unknown JSON fields, and
// requiring exactly the payload that matches the kind. It fails closed.
func ReadRequest(r io.Reader) (*Request, error) {
	kind, payload, err := readFrame(r, MaxRequestBytes)
	if err != nil {
		return nil, err
	}
	if !validKind(kind) {
		return nil, fmt.Errorf("controlproto: unknown request kind %#x", kind)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("controlproto: decode request: %w", err)
	}
	req.Kind = kind
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &req, nil
}

// Validate enforces that exactly the kind's payload is present and within
// bounds. A request carrying a payload for a different kind (or several)
// is malformed. Refs: MGIT-11.10.7 (security audit)
func (req *Request) Validate() error {
	set := map[byte]bool{
		KindLaunch: req.Launch != nil,
		KindExec:   req.Exec != nil,
		KindLand:   req.Land != nil,
		KindRemove: req.Remove != nil,
		KindStatus: req.Status != nil,
	}
	for k, present := range set {
		if present && k != req.Kind {
			return fmt.Errorf("controlproto: request kind %#x carries a %#x payload", req.Kind, k)
		}
	}
	switch req.Kind {
	case KindLaunch:
		if req.Launch == nil {
			return fmt.Errorf("controlproto: launch request missing payload")
		}
		return req.Launch.Validate()
	case KindExec:
		return validateExec(req.Exec)
	case KindLand:
		return requireTask(req.Land)
	case KindRemove:
		if req.Remove == nil || req.Remove.TaskID == "" {
			return fmt.Errorf("controlproto: remove request missing task_id")
		}
		return nil
	case KindStatus:
		return requireTask(req.Status)
	case KindList:
		return nil // no payload
	default:
		return fmt.Errorf("controlproto: unknown request kind %#x", req.Kind)
	}
}

// requireTask checks a TaskRef payload is present with a task ID.
func requireTask(ref *TaskRef) error {
	if ref == nil || ref.TaskID == "" {
		return fmt.Errorf("controlproto: request missing task_id")
	}
	return nil
}

// validateExec bounds an exec request's argv and env before it can drive
// a guest exec (defense-in-depth on the size of host-supplied input).
func validateExec(e *ExecArgs) error {
	if e == nil || e.TaskID == "" {
		return fmt.Errorf("controlproto: exec request missing task_id")
	}
	if len(e.Exec.Command) > MaxArgv {
		return fmt.Errorf("controlproto: argv length %d exceeds %d", len(e.Exec.Command), MaxArgv)
	}
	if len(e.Exec.Env) > MaxEnv {
		return fmt.Errorf("controlproto: env count %d exceeds %d", len(e.Exec.Env), MaxEnv)
	}
	for _, env := range e.Exec.Env {
		if len(env) > MaxEnvEntryBytes {
			return fmt.Errorf("controlproto: env entry %d bytes exceeds %d", len(env), MaxEnvEntryBytes)
		}
	}
	return e.Exec.Validate()
}

// WriteResponse frames and writes a response.
func WriteResponse(w io.Writer, resp *Response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("controlproto: encode response: %w", err)
	}
	if len(payload) > MaxResponseBytes {
		return fmt.Errorf("controlproto: response %d bytes exceeds %d cap", len(payload), MaxResponseBytes)
	}
	if err := writeLenPrefixed(w, payload); err != nil {
		return fmt.Errorf("controlproto: write response: %w", err)
	}
	return nil
}

// ReadResponse reads one response, enforcing the size ceiling before
// allocating and rejecting unknown fields.
func ReadResponse(r io.Reader) (*Response, error) {
	payload, err := readLenPrefixed(r, MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("controlproto: decode response: %w", err)
	}
	return &resp, nil
}

// writeFrame writes [kind][len BE][payload].
func writeFrame(w io.Writer, kind byte, payload []byte) error {
	var hdr [frameHeaderLen]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload))) //nolint:gosec // bounded by MaxRequestBytes
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

// readFrame reads one [kind][len BE][payload] frame, enforcing max before
// allocating. A clean EOF before any byte surfaces verbatim.
func readFrame(r io.Reader, max uint32) (kind byte, payload []byte, err error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > max {
		return 0, nil, fmt.Errorf("controlproto: message %d bytes exceeds %d cap", n, max)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("controlproto: truncated message body: %w", err)
	}
	return hdr[0], payload, nil
}

// writeLenPrefixed writes [len BE][payload].
func writeLenPrefixed(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // bounded by MaxResponseBytes
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readLenPrefixed reads [len BE][payload], enforcing max before allocating.
func readLenPrefixed(r io.Reader, max uint32) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > max {
		return nil, fmt.Errorf("controlproto: message %d bytes exceeds %d cap", n, max)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("controlproto: truncated message body: %w", err)
	}
	return payload, nil
}
