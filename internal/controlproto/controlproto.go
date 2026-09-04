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
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// Request kinds carried in the 1-byte frame tag.
//
// EVERY VALUE HERE MUST BE UNIQUE, and the compiler is the only thing that
// says so — a duplicate tag makes two verbs indistinguishable on the wire, so
// the daemon would dispatch one request as the other. MGIT-72 and MGIT-73 were
// developed in parallel and both reached for 'P'; it surfaced only when the two
// branches met. Adding a kind means reading this whole list first.
const (
	KindLaunch byte = 'L'
	KindExec   byte = 'X'
	KindLand   byte = 'D'
	KindList   byte = 'I'
	KindRemove byte = 'R'
	KindStatus byte = 'S'
	KindGrants byte = 'G' // list a task's pending capability requests
	KindGrant  byte = 'A' // approve one pending capability request
	KindSync   byte = 'Y' // re-stage a task's host worktree into its running guest
	// KindPolicySet replaces a RUNNING sandbox's egress allowlist without
	// relaunching it; an empty entry list is a full revoke (MGIT-72).
	KindPolicySet byte = 'P'
	// KindPolicyShow reports the allowlist a running sandbox is enforcing
	// right now, which after a mutation is NOT its launch-time policy.
	KindPolicyShow byte = 'Q'
	// KindExport brings a guest-built artifact out to a host-named path
	// (MGIT-73). 'F' for file, because 'P' and 'E' are already spoken for.
	KindExport byte = 'F'
	// KindEcho ('M') is the exact-size echo `mgit doctor` uses to exercise the
	// MGIT-160 response cap on the real channel. DECLARED in echo.go beside
	// the check it serves; it is a request kind like every other one here.
	//
	// KindHello ('V') is the version handshake. It is DECLARED in handshake.go,
	// beside the compatibility rule it serves, but it is a request kind like
	// every other one here — adding a kind means reading both places, and
	// TestHello_KindTagIsUnique asserts the whole set is distinct. Refs: MGIT-136
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
	// MaxPolicyEntries caps a replacement egress allowlist. A real allowlist
	// is a short list of host:port entries; anything larger is malformed or
	// hostile, and this daemon supervises every VM. Refs: MGIT-72
	MaxPolicyEntries = 1024
	// MaxPolicyEntryBytes caps one allowlist entry.
	MaxPolicyEntryBytes = 512
	// MaxPathBytes caps one filesystem path carried in a request (both the
	// guest source and the host destination of an artifact export). Well above
	// any real path, and bounded so a crafted message cannot drive a large
	// allocation or a pathological path walk. Refs: MGIT-73
	MaxPathBytes = 4096
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

// GrantArgs approves one pending capability request for a task's sandbox,
// identified by its key (the host-observed "ip:port" for egress). Refs: FR-17.12
type GrantArgs struct {
	TaskID string `json:"task_id"`
	Key    string `json:"key"`
}

// SyncArgs re-stages a task's host worktree into its RUNNING sandbox, or —
// with DryRun — asks only for the classification. The options travel as the
// model type so the CLI, the daemon and the backend cannot disagree about what
// a flag means. Refs: MGIT-76
type SyncArgs struct {
	TaskID string                    `json:"task_id"`
	Sync   model.WorktreeSyncOptions `json:"sync,omitempty"`
}

// PendingGrant is one capability request awaiting operator approval, carrying
// only host-observed facts (SEC-05): the capability, the real destination the
// host saw, and the Key to approve it with. Refs: FR-17.12, SEC-05
type PendingGrant struct {
	Capability string `json:"capability"`
	DestIP     string `json:"dest_ip,omitempty"`
	DestPort   int    `json:"dest_port,omitempty"`
	Key        string `json:"key"`
}

// PolicyArgs replaces a RUNNING sandbox's egress allowlist (MGIT-72).
//
// REVOKE IS AN EMPTY SET, deliberately: one verb means a caller cannot get
// "set" and "revoke" out of step, and there is no separate code path where a
// revoke could quietly become a no-op.
//
// Drain leaves ESTABLISHED flows to finish. It is opt-in because the default
// has to be the safe one: a caller who revokes registry egress and then runs
// untrusted code expects the grant gone, and a draining connection is exactly
// the exfiltration channel they just revoked. Refs: MGIT-72, ADR-012, SEC-04
type PolicyArgs struct {
	TaskID  string   `json:"task_id"`
	Entries []string `json:"entries,omitempty"`
	Drain   bool     `json:"drain,omitempty"`
}

// PolicyResult reports what a policy operation actually produced — the entries
// now IN FORCE (not the ones requested), how many rules they compiled to, and
// what the change did to established flows. Outcomes, not intentions.
// Refs: MGIT-72
type PolicyResult struct {
	Entries   []string `json:"entries"`
	RuleCount int      `json:"rule_count"`
	Killed    int      `json:"killed"`
	Drained   bool     `json:"drained"`
	// Pending says these entries are NOT being enforced yet: the sandbox is
	// registered but its microVM has not booted (lazy provisioning, FR-17.10),
	// so this is the policy it WILL boot with. It crosses the wire because a
	// caller that could not tell a staged policy from an enforced one would
	// run untrusted code believing a line is being held that nothing is
	// holding yet. Refs: MGIT-109, FR-17.10, SEC-04
	Pending bool `json:"pending,omitempty"`
}

// GrantResult confirms an approved grant (host-observed destination). Refs: FR-17.12
type GrantResult struct {
	Capability string `json:"capability"`
	DestIP     string `json:"dest_ip,omitempty"`
	DestPort   int    `json:"dest_port,omitempty"`
}

// ExportArgs names one guest->host artifact export. BOTH paths are supplied by
// the host-side caller — the guest names neither, which is what keeps this from
// being a guest-controlled write primitive against the host filesystem.
// Refs: MGIT-73, ADR-011
type ExportArgs struct {
	TaskID string                      `json:"task_id"`
	Export model.ArtifactExportRequest `json:"export"`
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
	Grants *TaskRef                    `json:"grants,omitempty"`
	Grant  *GrantArgs                  `json:"grant,omitempty"`
	Sync   *SyncArgs                   `json:"sync,omitempty"`
	// PolicySet mutates a running sandbox's egress allowlist; PolicyShow
	// reads the one in force (MGIT-72).
	PolicySet  *PolicyArgs `json:"policy_set,omitempty"`
	PolicyShow *TaskRef    `json:"policy_show,omitempty"`
	Export     *ExportArgs `json:"export,omitempty"`
	// Hello states the client's wire version before any verb is transacted.
	// It is the FIRST frame a current client sends on every connection; a
	// peer that sends anything else first is, by construction, a build from
	// before the handshake existed. Refs: MGIT-136
	Hello *HelloArgs `json:"hello,omitempty"`
	// Echo asks for a control response of an exact size, for `mgit doctor`
	// (MGIT-175). Declared in echo.go beside the verb it serves.
	Echo *EchoArgs `json:"echo,omitempty"`
	// SyncVerify asks whether a task's guest reads what was last delivered
	// to it (MGIT-164). Declared in syncverify.go beside the verb it serves.
	SyncVerify *TaskRef `json:"sync_verify,omitempty"`
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
	Error string `json:"error,omitempty"`
	// ErrorCode is a STABLE machine-readable token for WHY the op failed,
	// where the verb defines one (today: the egress-policy verbs' model.Egress*
	// codes). It travels beside Error rather than inside it because an
	// integrator that has to match on prose will break every time the prose
	// improves — which is exactly what happened to a consumer's pre-boot retry,
	// silently, twice. Empty for verbs with no code vocabulary.
	// Refs: MGIT-109, R-H233
	ErrorCode string              `json:"error_code,omitempty"`
	Sandbox   *model.SandboxInfo  `json:"sandbox,omitempty"` // launch, status
	List      []model.SandboxInfo `json:"list,omitempty"`    // list
	Landed    *LandResult         `json:"landed,omitempty"`  // land
	Pending   []PendingGrant      `json:"pending,omitempty"` // grants
	Granted   *GrantResult        `json:"granted,omitempty"` // grant
	// Synced carries a worktree sync's classification. It is set on SUCCESS
	// and, deliberately, on a conflict REFUSAL as well — alongside Error —
	// because a refusal that cannot name the paths it refused is not
	// actionable, and re-deriving them would cost a second round trip against
	// a tree that may have moved. Refs: MGIT-76
	Synced *model.WorktreeSyncReport `json:"synced,omitempty"` // sync
	Policy *PolicyResult             `json:"policy,omitempty"` // policy set/show
	// Exported reports one completed artifact export (MGIT-73).
	Exported *model.ArtifactExportResult `json:"exported,omitempty"`
	// Hello is the daemon's half of the version handshake. Only a client
	// that sent a hello ever receives one, so adding this field cannot reach
	// a pre-handshake client's strict decoder. Refs: MGIT-136
	Hello *HelloResult `json:"hello,omitempty"`
	// Echo is the exact-size answer to an echo request (MGIT-175).
	Echo *EchoResult `json:"echo,omitempty"`
	// SyncVerify is the guest's answer to a sync-verify request (MGIT-164).
	SyncVerify *SyncVerifyResult `json:"sync_verify,omitempty"`
}

// validKind reports whether k is a known request kind.
func validKind(k byte) bool {
	switch k {
	case KindLaunch, KindExec, KindLand, KindList, KindRemove, KindStatus, KindGrants, KindGrant,
		KindSync, KindPolicySet, KindPolicyShow, KindExport, KindHello, KindEcho, KindSyncVerify:
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
		KindGrants: req.Grants != nil,
		KindGrant:  req.Grant != nil,
		KindSync:   req.Sync != nil,

		KindPolicySet:  req.PolicySet != nil,
		KindPolicyShow: req.PolicyShow != nil,
		KindExport:     req.Export != nil,
		KindHello:      req.Hello != nil,
		KindEcho:       req.Echo != nil,
		KindSyncVerify: req.SyncVerify != nil,
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
	case KindGrants:
		return requireTask(req.Grants)
	case KindGrant:
		if req.Grant == nil || req.Grant.TaskID == "" || req.Grant.Key == "" {
			return fmt.Errorf("controlproto: grant request missing task_id or key")
		}
		return nil
	case KindSync:
		if req.Sync == nil || req.Sync.TaskID == "" {
			return fmt.Errorf("controlproto: sync request missing task_id")
		}
		return nil
	case KindPolicySet:
		return validatePolicySet(req.PolicySet)
	case KindPolicyShow:
		return requireTask(req.PolicyShow)
	case KindExport:
		return validateExport(req.Export)
	case KindHello:
		return validateHello(req.Hello)
	case KindEcho:
		return req.Echo.Validate()
	case KindSyncVerify:
		return requireTask(req.SyncVerify)
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

// validatePolicySet bounds a replacement allowlist before it can reach the
// compiler. An empty entry list is VALID — that is a full revoke, the whole
// point of the verb — so only the task ID and the size ceilings are enforced
// here; entry grammar is the allowlist compiler's job, and a policy that does
// not compile is rejected with the running one untouched. Refs: MGIT-72
func validatePolicySet(a *PolicyArgs) error {
	if a == nil || a.TaskID == "" {
		return fmt.Errorf("controlproto: policy request missing task_id")
	}
	if len(a.Entries) > MaxPolicyEntries {
		return fmt.Errorf("controlproto: policy entries %d exceeds %d cap",
			len(a.Entries), MaxPolicyEntries)
	}
	for _, e := range a.Entries {
		if len(e) > MaxPolicyEntryBytes {
			return fmt.Errorf("controlproto: policy entries: entry of %d bytes exceeds %d cap",
				len(e), MaxPolicyEntryBytes)
		}
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

// validateExport bounds an artifact-export request at the protocol boundary:
// the task must be named and both paths must be present, sane and bounded,
// before anything reaches the export engine's own containment checks.
// Refs: MGIT-73, SEC-03
func validateExport(e *ExportArgs) error {
	if e == nil || e.TaskID == "" {
		return fmt.Errorf("controlproto: export request missing task_id")
	}
	for name, path := range map[string]string{"guest_path": e.Export.GuestPath, "host_path": e.Export.HostPath} {
		if len(path) > MaxPathBytes {
			return fmt.Errorf("controlproto: export %s %d bytes exceeds %d", name, len(path), MaxPathBytes)
		}
	}
	return e.Export.Validate()
}

// ErrResponseTooLarge means a verb's answer exceeded MaxResponseBytes and was
// therefore never written. It is a SENTINEL rather than prose because the
// daemon must tell this failure from a dead connection: the first can still be
// answered with a small refusal, the second cannot be answered at all — and
// matching on a message would break the moment the message improves.
// Refs: MGIT-160, R-H233
var ErrResponseTooLarge = errors.New("control response too large to send")

// WriteResponse frames and writes a response.
func WriteResponse(w io.Writer, resp *Response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("controlproto: encode response: %w", err)
	}
	if len(payload) > MaxResponseBytes {
		return fmt.Errorf("%w: this verb's answer is %d bytes, over the %d-byte control-response "+
			"limit, so it could not be sent. Nothing was applied. Narrow what you asked for — "+
			"for a worktree sync, exclude large generated trees such as node_modules from the "+
			"worktree, or ask for a summary rather than a full path list",
			ErrResponseTooLarge, len(payload), MaxResponseBytes)
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
