package controlproto

import (
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// THE VERSION HANDSHAKE, and why this control plane needs one.
//
// Everything else in this package is frozen in BOTH directions. Requests and
// responses decode with DisallowUnknownFields, so a new request field makes an
// older daemon reject the request and a new response field makes an older
// client fail to decode; the exec stream's frame tags are a closed set, so a
// new frame kind is an "unexpected exec frame" to an older client. There is no
// field a peer can ignore and no frame it can skip. Any addition therefore
// breaks one direction, and the project has now paid for that four separate
// times (MGIT-76's Synced, MGIT-109's Pending and ErrorCode, MGIT-133's beat).
//
// The last of those paid the worst price: an mgit 0.5.0 CLI meeting a beating
// daemon reads the beat as an unknown exec frame, and its own classifier lands
// that in the "guest lost mid-command" phase, so a WIRE VERSION MISMATCH is
// reported to the operator as in-guest memory exhaustion — the MGIT-118
// misdiagnosis reached from a new direction (MGIT-136).
//
// So the peers state their versions before they transact, and a mismatch is
// settled at the handshake, ahead of every verb and ahead of every failure
// classifier that could reach a guest-shaped conclusion.
//
// WHERE THE VERSION SITS ON THE WIRE. The daemon's greeting line is
// deliberately UNCHANGED ("ok mgit-sandboxd\n", a fixed 17 bytes an older
// client reads with io.ReadFull and compares exactly). It cannot carry the
// version, and this was measured rather than assumed: any byte appended to the
// greeting is left unread in an older client's socket, where it is then
// misparsed as that client's response length or as its first exec frame, and
// any byte CHANGED inside it makes every verb fail as "daemon did not greet
// (not running, or a squatter holds the socket)" — a message that sends the
// reader hunting for a squatter. Worse, an older client's next move after the
// greeting depends on the verb it is about to send (a length-prefixed response
// for most, an execwire frame stream for exec), so no single eager byte
// sequence can be legible to both.
//
// The version is therefore exchanged in the frame immediately after the
// greeting, which is the first point at which the daemon knows who it is
// talking to: the client sends KindHello, the daemon answers with its own. A
// peer that does NOT send a hello is, by construction, a build from before
// this handshake existed, and the daemon can then refuse it in the shape that
// verb's client is waiting for — which is what finally makes a mixed pair say
// what it is instead of guessing at the guest.
//
// Refs: MGIT-136, MGIT-133, MGIT-118, MGIT-109, MGIT-76, FR-17.34
const (
	// ProtocolVersion is the control-plane wire version THIS build speaks.
	//
	// BUMP IT IN THE SAME COMMIT AS ANY WIRE CHANGE: a new request field, a
	// new response field, a new request kind, a new exec frame kind, or a
	// changed meaning for any of them. It is not a release number and it does
	// not track one; it counts wire-visible changes only, so a release with no
	// wire change leaves it alone.
	//
	// 2 is the first version to speak this handshake. It also covers the
	// MGIT-133 liveness beat, which is a wire addition in the same release.
	// 3 adds the echo verb (KindEcho, an Echo field on request and response)
	// that `mgit doctor` uses to exercise the MGIT-160 response cap
	// (MGIT-175): a daemon left running from a 2 build refuses at the
	// handshake, naming the restart, instead of answering "invalid request".
	//
	// 4 adds the sync-verify verb (KindSyncVerify, a SyncVerify field on
	// request and response) that `mgit doctor` uses to ask a guest whether it
	// reads what was last delivered to it (MGIT-164).
	ProtocolVersion = 4

	// LegacyProtocol names every mgit built before the handshake existed
	// (0.5.x and earlier). Such a peer never states a version, so this number
	// is ASSIGNED to it by observation — the absence of a hello — rather than
	// read off the wire. It is deliberately incompatible: those builds are the
	// ones whose failure mode this handshake exists to end.
	LegacyProtocol = 1

	// MaxVersionBytes caps a peer's self-reported build string. It is a human
	// label that lands in an error message, and this daemon supervises every
	// VM, so it is bounded like everything else here.
	MaxVersionBytes = 256
)

// KindHello is the request tag for the version handshake.
//
// 'V' for version. The tag lives beside the verb tags in controlproto.go's
// list — read that list before adding another — and
// TestHello_KindTagIsUnique pins it against all of them, because a hello
// indistinguishable from a verb would be dispatched as one. Refs: MGIT-136, MGIT-73
const KindHello byte = 'V'

// HelloArgs is what a peer says about itself: the wire version it speaks, and
// the build it came from. Refs: MGIT-136
type HelloArgs struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version,omitempty"`
}

// HelloResult is the daemon's half of the exchange, in the same shape.
// Refs: MGIT-136
type HelloResult struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version,omitempty"`
}

// Compatible reports whether a peer speaking protocol `peer` may transact with
// this build.
//
// THE RULE IS EXACT EQUALITY, and it is a choice, not a shortcut.
//
// The friendlier alternative — a minimum-supported version, so a range of
// builds interoperates — is a PROMISE, and this codec cannot keep it. Keeping
// it would mean every future wire addition stayed decodable by every peer in
// the supported range, which DisallowUnknownFields on both directions and a
// closed set of frame tags make impossible: the very next added field breaks
// the range the moment it is used. A range we cannot honor is worse than
// lockstep, because it converts a clean refusal into a partial pair that works
// until it silently does not — which is the exact history above.
//
// Lockstep is also what the deployment already is: mgit and mgit-sandboxd ship
// in ONE archive and one Homebrew formula and are installed together
// (internal/buildinfo says so at the top). The only way to end up mixed is a
// daemon left running across an upgrade, and that is a 5-second fix the
// mismatch message names.
//
// Refs: MGIT-136
func Compatible(peer int) bool { return peer == ProtocolVersion }

// Peer is one side of a mismatch, for rendering. Version is the peer's
// self-reported build, empty when it is too old to have stated one.
type Peer struct {
	Protocol int
	Version  string
}

// unknownBuild stands in for a peer that could not state its build. It says
// what is known ("too old to say") rather than inventing a version, and it
// keeps the rendered line free of empty parentheses.
const unknownBuild = "build not reported (too old to say)"

// build renders a peer's build for the message.
func (p Peer) build() string {
	if strings.TrimSpace(p.Version) == "" {
		return unknownBuild
	}
	return p.Version
}

// SkewMessage renders the one thing both ends print when the versions differ.
//
// It is single-sourced here because BOTH peers produce it — the client when it
// meets an older or newer daemon, the daemon when an older client reaches it —
// and two copies would drift into two different answers to the same question.
//
// The wording is load-bearing, in three ways:
//
//  1. It names the fault plainly ("CLI and daemon differ — upgrade both")
//     instead of printing two version numbers and leaving the reader to work
//     out that they are supposed to match.
//  2. It says WHICH SIDE is which build, because the fix depends on which one
//     is behind, and an agent reporting the failure needs both anyway
//     (MGIT-132 asks for exactly this field in bug reports).
//  3. It names commands the reader can actually run, one per route this
//     project is actually installed by — install.sh, Homebrew, go install, a
//     checkout build. A remedy that names a command the reader has no way to
//     run is the defect one level up (MGIT-132), and "upgrade both" is not
//     actionable on its own.
//  4. The FINAL action is aimed at the side that is actually stale, and only
//     that side — see staleSideRemedy.
//
// It says nothing whatsoever about the guest or its memory cap: a version
// mismatch is a fact about two host binaries, and pointing at the sandbox's
// memory is the MGIT-118/MGIT-136 mistake this whole ticket exists to end.
//
// Refs: MGIT-138, MGIT-136, MGIT-132, MGIT-118
func SkewMessage(cli, daemon Peer) string {
	var b strings.Builder
	// The headline IS the sentinel's text, so a mismatch that crosses the wire
	// as a bare string is still recognizable as one on the far side.
	b.WriteString(model.ErrSandboxVersionSkew.Error() + " — upgrade both.\n")
	fmt.Fprintf(&b, "  mgit CLI:      control protocol %d, %s\n", cli.Protocol, cli.build())
	fmt.Fprintf(&b, "  mgit-sandboxd: control protocol %d, %s\n", daemon.Protocol, daemon.build())
	b.WriteString(
		"The two binaries ship together and speak one wire version, so a mixed pair is refused\n" +
			"here rather than allowed to fail later as something it is not.\n" +
			"Upgrade both, by whichever route you installed them:\n" +
			"  install.sh:   curl -fsSL https://raw.githubusercontent.com/hyper-swe/mgit/main/install.sh | sh\n" +
			"  Homebrew:     brew upgrade hyper-swe/tap/mgit\n" +
			"  go install:   go install github.com/hyper-swe/mgit/cmd/mgit@latest && \\\n" +
			"                go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest\n" +
			"  from a clone: go build -o <bindir>/mgit ./cmd/mgit && \\\n" +
			"                go build -o <bindir>/mgit-sandboxd ./cmd/mgit-sandboxd\n")
	b.WriteString(staleSideRemedy(cli, daemon))
	b.WriteString(
		"Confirm both, and quote both in any bug report:  mgit --version; mgit-sandboxd --version")
	return b.String()
}

// staleSideRemedy renders the closing action for the side that is actually
// behind — and only for that side.
//
// The handshake exchanges BOTH versions, so every peer rendering this message
// already knows which binary is stale. Until MGIT-138 neither used it: both
// ends closed with "stop the daemon still running the old build: pkill -f
// mgit-sandboxd". That is exact when the DAEMON is the old half. It is
// superfluous when the CLI is — the direction verified live, where the daemon
// was already the new build — and it sends the reader after a process that is
// not the problem while the actual fix (upgrade the CLI) goes unstated.
//
// A remedy aimed at the wrong side is a mild member of the misdiagnosis family
// this project has now closed four routes into (MGIT-104, MGIT-108, MGIT-118,
// MGIT-136). Harmless is not the same as right, and the information needed to
// be right was already on the wire.
//
// Equal protocol numbers are not a mismatch and cannot reach here (Compatible
// is equality, and this message is rendered only when it fails). If one ever
// did, NEITHER side is named stale — an unaimed remedy beats a guessed one.
// Refs: MGIT-138, MGIT-136, MGIT-132
func staleSideRemedy(cli, daemon Peer) string {
	switch {
	case daemon.Protocol < cli.Protocol:
		// SCOPED TO THIS REPO, deliberately. A bare `pkill -f mgit-sandboxd`
		// matches EVERY repository's daemon on the host, not just the stale one
		// this message is about: sockets are keyed per repo root, so a developer
		// with several mgit checkouts loses unrelated daemons and the microVMs
		// they are supervising. Telling a reader to do that is the same defect
		// this message exists to avoid -- a remedy that damages what it was not
		// aimed at -- and it is the exact hazard this project's own working
		// rules forbid ("never kill by name alone"). Refs: MGIT-141, MGIT-138
		return "Then stop the stale daemon. It is the one serving THIS repository, so scope the\n" +
			"kill to it rather than to every mgit daemon on the host:\n" +
			"  pkill -f \"mgit-sandboxd.*--repo-root $(git rev-parse --show-toplevel)\"\n" +
			"A bare `pkill -f mgit-sandboxd` would also stop other repositories' daemons and\n" +
			"the sandboxes they are running.\n"
	case cli.Protocol < daemon.Protocol:
		return "The stale half here is the mgit CLI: the running mgit-sandboxd is already the newer\n" +
			"build, so there is no old daemon to stop — upgrading the CLI is the whole fix.\n"
	default:
		return ""
	}
}

// validateHello bounds a hello before its version can reach the compatibility
// rule. A protocol number below 1 is not a version any build ever spoke.
// Refs: MGIT-136
func validateHello(h *HelloArgs) error {
	if h == nil {
		return fmt.Errorf("controlproto: hello request missing payload")
	}
	if h.Protocol < LegacyProtocol {
		return fmt.Errorf("controlproto: hello protocol %d is not a version", h.Protocol)
	}
	if len(h.Version) > MaxVersionBytes {
		return fmt.Errorf("controlproto: hello version %d bytes exceeds %d",
			len(h.Version), MaxVersionBytes)
	}
	return nil
}
