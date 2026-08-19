# IDD — FR-17 Sandbox Host↔Guest Protocol (Interface Design Document)

**Status:** Living document (MIL-STD-498 IDD-equivalent, FR-17.35)
**Refs:** FR-17.5, FR-17.6, FR-17.24, FR-17.27, FR-17.28, FR-17.35, FR-17.38; ADR-002 (dual hash), ADR-005 (sandbox), AUDIT-FR17-SANDBOX-SECURITY-V1.md (SEC-01, SEC-06, SEC-10, SEC-11), AUDIT-FR17-SANDBOX-V1.md (F-02, F-03, F-09)

FR-17.35 requires this specification to exist **before** the protocol's
backend implementation. Each section is marked with the issue that
authors/implements it; a section is normative once its issue is `done`.

## 0. Trust model (recap)

The **guest is the hostile party** — it runs untrusted third-party code.
The **host (`mgit-sandboxd`) is the trust anchor**. `mgit-guest` is
**pure transport**: it frames bytes over the channel and holds **no
signing material** (SEC-01). Every integrity decision is made host-side
on bytes the host itself read.

## 1. Transport & framing — *MGIT-11.8.2* (normative)

Control/land messages travel over the per-sandbox vsock channel
(AF_VSOCK on KVM/Virtualization.framework; AF_HYPERV/HvSocket on the
future WCOW backend, FR-17.27).

### 1.1 Object framing

A land payload is a stream of object frames. Each frame is:

```
[1 byte: object type][4 bytes: big-endian uint32 payload length][payload]
```

Object type is one of `C` (commit), `T` (tree), `B` (blob); any other
tag is a schema violation. The stream ends at EOF on a frame boundary; a
truncated header or body is a schema violation. All multi-byte integers
are big-endian. Durations/timestamps elsewhere in the protocol are wire
int64 nanoseconds (never JSON doubles — they exceed 2^53 at ~104 days).

### 1.2 Ceilings (FR-17.35; host-configurable per FR-17.13)

| Ceiling | Default | Enforcement |
|---|---|---|
| Per-object size | **64 MiB** | declared length checked **before** the body is read (zip-bomb defense) |
| Objects per land | **100 000** | running count as the stream decodes |
| Total land payload | **4 GiB** | running aggregate of declared lengths |

Exceeding any ceiling, an unknown object type, or truncated framing
yields `ErrLandVerificationFailed`; nothing partial is imported.

### 1.3 Tree-entry path rules (NFR-5.6 at the land boundary, T8)

Every tree-entry path MUST be a canonical, relative, slash-separated,
worktree-confined path. The host rejects (`ErrLandVerificationFailed`):
empty paths; absolute paths; any `..` component (traversal); and
non-canonical encodings — `.` components, `//` (empty components),
trailing `/`, backslashes, and NUL. Canonicality is `path.Clean(p) == p`
plus an explicit leading-`..` check (Clean preserves a leading `..`).

### 1.4 Audit-bound strings

Guest-supplied strings destined for audit tables (e.g. commit message,
author) are length- and control-char-sanitized at the store insert
boundary (F-09, `internal/store/index`), which is the authoritative
sanitization point; the land protocol additionally bounds object sizes
above. Land import (MGIT-11.8.5) routes guest strings through that
sanitizer when it composes audit records.

## 2. CID / peer binding — *MGIT-11.8.6* (normative)

Each guest's control/land/attestation channel is bound at launch to its
hypervisor peer identity — the vsock CID (AF_VSOCK: KVM,
Virtualization.framework) or the VM-GUID (AF_HYPERV: WCOW). The host
`PeerBinder` (`internal/sandboxd`) holds an opaque peer-identity string
per sandbox so one binder serves every backend. On every incoming
connection the daemon calls `Authorize(addressedSandboxID, sourcePeer)`,
which **fails closed**:

- no binding for the addressed sandbox (never launched, or torn down) → reject
- empty/unverifiable source peer → reject
- source peer ≠ the sandbox's bound identity → reject

so one guest can never reach another's channel (SEC-10). Every rejection
is audited (`event=peer_rejected`, with the host-observed source peer —
never guest-asserted text, SEC-05). At teardown the binding is
**invalidated** (`Invalidate`), so a CID/GUID the hypervisor recycles for
a successor VM cannot inherit a destroyed sandbox's binding (FR-17.27).
The transport supplies the source peer identity (the vsock connection's
remote CID / the HvSocket VM-GUID); reading it is the backend adapter's
job, wired with the sandbox service (MGIT-11.9).

## 3. Commit attestation — *MGIT-11.8.1* (normative)

### 3.1 Issuance (host-side only, SEC-01)

As commit objects cross the land channel, `mgit-sandboxd` recomputes
both hashes from the bytes it read (§4 hash-on-write) and issues an
`Attestation` binding the commit to the sandbox that produced it. The
guest cannot issue one: it holds no key. `Attest` is **not** a
sign-anything oracle — the daemon issues only for `(sandboxID, commitHash,
contentHash)` triples it observed crossing that sandbox's channel.

### 3.2 Attestation message

```json
{
  "sandbox_id":     "<ULID>",
  "commit_hash":    "<40 lowercase hex, git SHA-1>",
  "content_hash":   "<64 lowercase hex, mgit SHA-256 (ADR-002)>",
  "base_digest":    "<sha256:<64 hex> — the guest base the sandbox booted; omitted when the host recorded none>",
  "payload_version":"<integer signing-payload layout; omitted means 1>",
  "alg":            "ed25519",
  "key_id":         "<64 hex: SHA-256 fingerprint of the host public key>",
  "host_signature": "<base64 std (RFC 4648 §4, padded) of the 64-byte Ed25519 signature>",
  "issued_at":      "<RFC3339Nano UTC; host receive-time, advisory display only>"
}
```

`issued_at` carries the host receive-time (SEC-11/FR-17.28). The JSON
string form is for transport/display; it is **not** the bytes signed.

`base_digest` names the environment that produced the commit (MGIT-61.15).
It is host-established — read from the launch record, never guest-asserted —
and it is signed, as `payload_version` 2 below. `payload_version` selects
which byte layout §3.3 produced, because the set of facts a host attests
grows over time and records signed under an earlier layout must keep
verifying.

### 3.3 Canonical signing payload (byte-stable)

The signature input is a **deterministic, length-prefixed field
concatenation** — never re-serialized JSON (Go `time.Time` RFC3339
fractional seconds and cross-language base64 variants are not
byte-stable, per the MGIT-11.2 security pass). Each field is encoded as
an 8-byte big-endian length followed by the field bytes, in this exact
order:

**Layout 1** (`payload_version` absent or `1`) — the original, and what every
attestation issued before MGIT-61.15 was signed over:

1. `sandbox_id`            (UTF-8)
2. `commit_hash`           (UTF-8, the 40-hex string)
3. `content_hash`          (UTF-8, the 64-hex string)
4. `key_id`                (UTF-8, the 64-hex fingerprint)
5. `issued_at_unix_nano`   (the 8 raw big-endian bytes of `IssuedAt.UTC().UnixNano()`, an int64 — **not** a decimal string, **not** the RFC3339 form)

**Layout 2** (`payload_version` = `2`) — layout 1's bytes **exactly**, then:

6. `payload_version`       (the 8 raw big-endian bytes of the version, an int64 — so the payload is self-describing)
7. `base_digest`           (UTF-8, length-prefixed; a zero length when the host recorded no base)

A later layout MUST likewise append to the previous layout's bytes and MUST
NOT reorder or remove a field, so that a record signed under any earlier
layout still hashes to precisely what was signed. A verifier MUST select the
layout by EXACT version match, never "at least": treating a future layout as
this one would verify a signature over bytes it does not describe.

The host signs this payload with Ed25519. Binding `key_id` into the
payload rules out algorithm/key confusion across rotations. Length
prefixing rules out field-boundary collisions.

### 3.4 Verification

`Verify` MUST, in order:
1. `Attestation.Validate()` (structural shape). This MUST reject an
   unrecognized `payload_version`, and MUST reject a `base_digest` present
   under a layout that does not sign it — otherwise a record signed under
   layout 1 can be given an environment claim that no signature covers, the
   mirror image of stripping a signed field.
2. Reject any `alg` other than `ed25519` (no algorithm agility in v1).
3. Resolve the public key for `key_id`. The independent verifier
   (FR-17.32) obtains it from the SANDBOX-IMAGES.md register, **not** from
   the policy store it audits (FR-17.38). `mgit-sandboxd` resolves it
   from its host trust anchor; a `key_id` that is neither the current nor
   a known rotated fingerprint is rejected.
4. Recompute the §3.3 payload from the attestation fields and verify the
   Ed25519 signature. Any field tampered after signing fails here.

### 3.5 Key management (FR-17.38)

- The attestation signing key is generated **host-side**, stored under
  the host config root with **0600** perms, in a file **separate from**
  `images.lock`, the policy store, **and** the image-signing trust root
  (a distinct key per purpose).
- The private key **never** enters a guest or an image.
- Rotation appends an audit event recording the **old and new**
  fingerprints; the prior public key is retained so attestations issued
  under it still verify (`key_id` selects the key).
- `key_id` is the hex SHA-256 fingerprint of the Ed25519 public key.
- Retired public keys are retained **indefinitely** (no prune path).
  This is intentional: an append-only audit posture requires that any
  attestation ever issued remain verifiable for the life of the record.

## 4. Hash-on-write dual-hash verification — *MGIT-11.8.3*

Land verification hashes the **exact buffer it imports** (a single read,
hashed and written from the same bytes) — never a second fetch (SEC-06).
Both ADR-002 hashes (SHA-1 git object id, SHA-256 `content_hash`) are
recomputed on those bytes; mismatch → `ErrLandVerificationFailed`.

## 5. require_sandbox enforcement — *MGIT-11.8.4* (normative)

`require_sandbox` defaults **true** (safety-critical). The land gate
(`land.EnforceRequireSandbox`) returns the `task_commits.sandbox_id` to
record (`*string`; nil = SQL NULL) or refuses the commit:

| Policy | Attestation | Outcome |
|---|---|---|
| on | none | refuse — `ErrUnattestedCommit` |
| on | present but invalid | refuse — `ErrAttestationInvalid` (forged never lands) |
| on | valid | land with `sandbox_id = att.SandboxID` |
| off | (not consulted) | land with `sandbox_id = NULL` — the permanently visible F-02/SEC-02 gap |

Policy-off always records NULL (the attestation is not consulted): a
non-NULL `sandbox_id` therefore unambiguously means "produced and
attested under enforced sandboxing." Disabling the policy is itself an
audited event (FR-17.6).

## 6. Atomic land import — *MGIT-11.8.5* (normative)

Land is all-or-nothing (squash semantics, FR-2.x). `land.Lander.Land`
runs three phases in order, and that order is the atomicity guarantee:

1. **Import objects** for every commit (idempotent, content-addressed).
   A failure here aborts before any `task_commits` row exists — objects
   already written are harmless orphans, never a partial land.
2. **Append the batch** — every commit's `task_commits` row in ONE
   serialized transaction (`index.Store.AppendTaskCommits`); a failure
   mid-batch rolls back the whole batch. This is the commit point. The
   row's `created_at` is the host **receive-time**; the guest's own
   timestamp stays advisory inside the git object (SEC-11, FR-17.28).
   The `sandbox_id` from the require_sandbox gate (§5) is recorded here
   (NULL when unsandboxed).
3. **Fast-forward** the task branch to the last commit, **append-only**
   (the `Brancher` refuses a non-fast-forward — land never rewrites).

The object import, atomic append, and branch update are injected ports,
so the trust-boundary logic is host-side and testable independent of
go-git/SQLite wiring (which the sandbox service composes, MGIT-11.9).

## 7. Exec-stream liveness — *MGIT-133* (normative)

This section governs the **CLI ↔ `mgit-sandboxd`** leg, not the
host↔guest leg above. Both legs frame exec traffic with
`internal/execwire`, but only this one carries liveness beats: the guest
neither sends nor sees them.

### 7.1 Why a beat and not a timeout

`serveExec` does not stream. It calls the service, waits for a whole
`ExecResult`, and only then relays output. So between the request and
the command's end the control socket carries **nothing**, and any wall
clock measured over that read is a cap on how long a build may take.
That is precisely what it was: a deadline armed at dial ended every exec
at 30 s, reported to the agent as in-guest memory exhaustion (MGIT-122,
MGIT-118). Removing it fixed the wrong kill and created a silent wedge —
a daemon that is alive but stuck became indistinguishable from a guest
doing slow work.

A beat resolves both because it bounds **silence**, not **duration**. A
build that emits nothing for an hour keeps the stream alive for that
hour; a daemon that stops answering falls silent and is caught in
seconds. A blanket `--timeout` cannot do this at any value: whatever is
chosen, some legitimate build exceeds it and dies for running long
rather than for being stuck.

### 7.2 Frame

```
[1 byte: 'H'][4 bytes: big-endian uint32 length = 0]
```

`FrameHeartbeat` = `'H'`, distinct from `'O'` / `'E'` / `'R'` and
asserted so by `TestFrameKinds_AreUnique_NoTagCollision` — two branches
once collided on a control tag (MGIT-73), and on this stream a collision
would write beats into the caller's output or end the stream early.

The payload is **always empty**. A beat asserts one thing and must not
invite a reader to conclude anything else from it. It is also
**host-generated only**: guest frames arrive on a different connection
and are relayed as stdout/stderr/result, so a hostile guest cannot forge
liveness (SEC-05).

### 7.3 What a beat asserts — and what it does not

A beat means, exactly: the daemon process is scheduled, this exec's
handler goroutine is running, this connection is writable, and the
sandbox service answered for this exec's own sandbox within the last
interval.

It does **not** mean the guest is making progress. Nothing host-side can
assert that: the daemon relays a command's output only on completion, so
"backend call still running" and "backend call will never return" are
the same observable state here. A caller who wants to bound duration
asks for it with `ExecRequest.Timeout` (`--timeout`, default unbounded).

**Every beat is gated on a probe**, and that gate is the design. The
daemon runs one prober goroutine calling `Service.Status` for the exec's
own task — which takes the sandbox registry mutex every state transition
passes through — and a beat is written only against a fresh answer, at
most one per answer. A free-running ticker would beat straight through a
registry deadlock and certify a liveness that does not exist, which is
worse than sending nothing.

### 7.4 Timing

| Constant | Value | Meaning |
|---|---|---|
| `execwire.HeartbeatInterval` | 5 s | daemon beat cadence |
| `execwire.HeartbeatMisses` | 3 | consecutive beats a client tolerates |
| `execwire.StallTimeout` | 15 s | client idle deadline between frames |

Both ends read these from `execwire` so the cadence and the judgement
cannot drift apart. More than one miss is allowed because a busy daemon
(a concurrent teardown holding the registry) can legitimately skip a
beat; the total stays in seconds because a wedge must be caught fast.

### 7.5 Client behaviour

The client arms `StallTimeout` before every frame read; **every** frame —
beat, output, or result — rearms it, so the clock measures the gap
between frames.

- **Beat then silence** → the daemon has proved its silence is
  meaningful. The exec fails with `model.ErrSandboxDaemonUnresponsive`,
  whose message names the daemon as the suspect and explicitly clears
  the command and the guest. The CLI classifies this as
  `phaseDaemonStalled` and prints **no** memory-cap advisory — a
  daemon-side stall reported as a guest lost mid-command is MGIT-118's
  misdiagnosis on a new cause.
- **Never beat** → the same verdict, and this is MGIT-138's change. A
  peer only reaches this loop by completing the §8 handshake at this
  build's protocol, which *covers* the beat, and the daemon writes its
  first beat before it calls the service — so a peer here owes one inside
  the first window whatever the command is doing. Silence is therefore a
  daemon that promised beats and stopped answering.

  MGIT-133 read this case as "an `mgit-sandboxd` predating MGIT-133":
  the deadline was dropped, MGIT-122's unbounded wait restored, and a
  one-line notice printed. MGIT-136 made that reading unreachable — a
  daemon too old to beat is too old to handshake and is refused at
  `dialGreeted` — and in the one case that still reached it, a current
  daemon wedged before its first beat, it was wrong twice: it named the
  daemon *old* when it was *hung*, and it reinstated the unbounded wait
  §7 exists to end. A peer that legitimately cannot beat is a wire
  change, and a wire change bumps `ProtocolVersion` and is refused one
  layer up. The fallback, the notice, and the `beating` flag are gone.
- **Cancellation** → `watchCancel` expires the connection rather than
  closing it, so a Ctrl-C reaches the read as `os.ErrDeadlineExceeded`
  too. The context is checked first; the caller's own withdrawal is
  never reported as a daemon stall.

### 7.6 Compatibility

The first beat is written **before** the service is called, so a client
that got past the handshake is owed one immediately (§7.5). MGIT-133
shipped that beat as a *capability advertisement* — its absence
identified an older daemon — which is how **new CLI → old daemon**, the
common mixed pair, was kept working: the daemon is long-lived, so an
upgrade leaves the previous release's daemon serving until it idles out.
Since MGIT-136 that pair is settled at the handshake instead, and the
first beat's remaining job is to make the client's first idle window
judgeable (MGIT-138).

The other direction breaks, and the choice was forced. This control
plane has **no capability negotiation available at all**: `controlproto`
decodes requests *and* responses with `DisallowUnknownFields`, so a new
field in either breaks the opposite peer; the greeting is a fixed
17-byte constant read with `io.ReadFull`, so extending it desynchronizes
an old client's next read. Any addition must pick a direction to break,
and this one keeps the common pair working.

Measured on macOS, mgit v0.5.0 CLI against an MGIT-133 daemon: `sandbox
list`, `sandbox status` and `run --check` still work; `mgit run` fails
with "unexpected exec frame 0x48" — and the old binary classifies that
as a guest lost mid-command and prints its memory-cap advisory, so a
version mismatch reads as in-guest memory exhaustion. That misdiagnosis
lives in a released binary and cannot be fixed from this side. **MGIT-136
tracks giving the control plane a forward-compatible seam** so the next
addition does not face the same choice.

**MGIT-136 closes this**, and §8 is the rule that replaces the paragraph
above: the peers now state their wire versions before they transact, an
addition to this wire is a **same-release change** enforced by a version
number rather than by convention, and a mixed pair is refused with a
message that names both builds instead of failing later as something it
is not.

---

## 8. Control-plane version handshake — *MGIT-136* (normative)

### 8.1 Why a handshake and not a negotiation

`controlproto` has no forward-compatible seam and cannot cheaply be given
one: requests **and** responses decode with `DisallowUnknownFields`, so a
new field in either direction breaks the opposite peer, and the exec
stream's frame tags are a closed set, so a new frame kind is an
"unexpected exec frame" to an older client. There is no field a peer can
ignore and no frame it can skip.

So the peers do not negotiate capabilities. They state versions, compare
them, and refuse to transact when they differ — early, and before any
failure classifier can reach a guest-shaped conclusion about it.

### 8.2 Sequence (normative)

```
daemon  -> "ok mgit-sandboxd\n"                    greeting, UNCHANGED
client  -> KindHello 'V' {protocol, version}       first frame, always
daemon  -> Response{Hello{protocol, version}}      (+ error when they differ)
client  -> one verb                                only if the versions match
```

Peer-UID authentication is unchanged and still precedes every byte of
this. The greeting still precedes the handshake, so the squatter defense
and the activation liveness probe (`dialOK`) are untouched.

### 8.3 Why the version is not in the greeting line

The greeting is a fixed 17-byte constant that an mgit ≤ 0.5.x client
reads with `io.ReadFull` and compares exactly. Both ways of putting a
version in it were measured against the shipped v0.5.0 binary:

| change | what a 0.5.x client does |
|---|---|
| append bytes after the greeting | accepts the greeting, then reads our bytes as its own response length (`message 1886545268 bytes exceeds 1048576 cap`) or as its first exec frame |
| change bytes inside the greeting | rejects it, spawns its own daemon, and fails every verb with `sandbox daemon unavailable … not dialable after spawn` |

Neither is legible, and no single eager byte sequence can be: after the
greeting an old client's next read depends on the verb it is about to
send — a length-prefixed control response for every verb, an `execwire`
frame stream for exec. The daemon only learns which when the request
arrives, so the version is exchanged in the frame after the greeting.

### 8.4 Compatibility rule (normative)

**Exact equality.** A peer may transact iff its `protocol` equals this
build's `controlproto.ProtocolVersion`. Anything else — one below, one
above, or absent — is refused.

`ProtocolVersion` is **2**. Protocol **1** is assigned by observation to
every build that predates the handshake (mgit ≤ 0.5.x): such a peer never
states a version, and the absence of a hello is what identifies it.

`ProtocolVersion` MUST be bumped **in the same commit** as any wire
change: a new request field, a new response field, a new request kind, a
new exec frame kind, or a changed meaning for any of them. It is not a
release number and does not track one.

A minimum-supported-version rule was considered and rejected. It is a
promise that a range of builds interoperates, and this codec cannot keep
that promise: the next added field breaks the range the moment it is
used. A range that cannot be honoured is worse than lockstep, because it
turns a clean refusal into a pair that works until it silently does not —
which is the entire history in §7.6. Lockstep is also the deployment
reality: both binaries ship in one archive and one Homebrew formula and
are installed together.

### 8.5 Refusing a pre-handshake peer

A peer that sends a verb where its hello belongs speaks protocol 1. It is
refused **in the shape that verb's client is waiting for**:

| request kind | refusal |
|---|---|
| `KindExec` | an `execwire` stderr frame carrying the whole message, then a terminal result frame (`exit_code: -1`) naming the skew |
| every other kind | one `controlproto.Response` with the message in `error` |

This is what lets an mgit 0.5.x operator read the real reason. It also
means an old `mgit run` and an old `mgit sandbox exec` cannot reach their
memory-cap advisory: `run` fails while listing sandboxes, and `sandbox
exec` only prints that advisory when a follow-up `Status` call succeeds,
which it no longer does.

### 8.6 The message (normative content, not wording)

Single-sourced in `controlproto.SkewMessage`, produced identically by
both peers for the same pair of versions, and it MUST:

- open with `model.ErrSandboxVersionSkew`'s text, so the conclusion
  survives a crossing that carries only a string (an exec result frame
  has no place for an error identity);
- name **which side is which build** — the CLI's and the daemon's, with
  their protocol numbers — because the fix depends on which is behind and
  because MGIT-132 asks for exactly those fields in a bug report;
- name a command per install route this project actually ships:
  `install.sh`, `brew upgrade hyper-swe/tap/mgit`, `go install …`, and a
  clone build; and end with `mgit --version` / `mgit-sandboxd --version`
  to confirm and to quote in a bug report (MGIT-132);
- **aim its closing action at the side that is actually stale, and only
  that side** (MGIT-138). Both versions are on the wire, so both peers
  know which binary is behind. When the DAEMON is the older half,
  `pkill -f mgit-sandboxd` releases the daemon left running across the
  upgrade and the message says so. When the CLI is the older half — the
  direction verified live, an mgit 0.5.0 CLI meeting a current daemon —
  that line MUST NOT appear: there is no stale daemon to stop, and
  upgrading the CLI is the whole fix. Equal protocol numbers are not a
  mismatch and cannot reach this message; if one ever did, NEITHER side
  is named stale. A remedy aimed at the wrong side is a mild member of
  the misdiagnosis family below, and harmless is not the same as right;
- say **nothing** about a guest, a sandbox, or a memory cap. A mismatch
  is a fact about two host binaries. Pointing at the cap is the
  MGIT-118 misdiagnosis, and MGIT-136 was the fourth route into it.

### 8.7 What is, and is not, evidence of a version

A transport failure — a closed socket, a timeout, an undecodable reply —
says **nothing** about a peer's version and is reported as itself. Only a
daemon that *answered* is judged: an answer with no `hello` in it comes
from a build that does not know the verb (pre-handshake), and an answer
with a different protocol number says so directly.

On the CLI side the mismatch is settled in `classifyGuestFailure` as
`phaseVersionSkew`, **ahead of** `phaseDaemonStalled` and ahead of every
guest phase, so it can never fall through to the memory-cap advisory.
