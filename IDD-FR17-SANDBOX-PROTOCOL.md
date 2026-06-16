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

## 2. CID / peer binding — *MGIT-11.8.6*

Each guest's channel is bound at launch to its hypervisor peer identity
(vsock CID, or VM-GUID on HvSocket). The daemon rejects and audits any
message whose source peer identity differs from the addressed sandbox's
binding, so one guest can never reach another's land/attestation channel
(SEC-10). The binding is invalidated at teardown: a recycled CID/GUID
MUST NOT inherit a destroyed sandbox's binding.

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
  "sandbox_id":    "<ULID>",
  "commit_hash":   "<40 lowercase hex, git SHA-1>",
  "content_hash":  "<64 lowercase hex, mgit SHA-256 (ADR-002)>",
  "alg":           "ed25519",
  "key_id":        "<64 hex: SHA-256 fingerprint of the host public key>",
  "host_signature":"<base64 std (RFC 4648 §4, padded) of the 64-byte Ed25519 signature>",
  "issued_at":     "<RFC3339Nano UTC; host receive-time, advisory display only>"
}
```

`issued_at` carries the host receive-time (SEC-11/FR-17.28). The JSON
string form is for transport/display; it is **not** the bytes signed.

### 3.3 Canonical signing payload (byte-stable)

The signature input is a **deterministic, length-prefixed field
concatenation** — never re-serialized JSON (Go `time.Time` RFC3339
fractional seconds and cross-language base64 variants are not
byte-stable, per the MGIT-11.2 security pass). Each field is encoded as
an 8-byte big-endian length followed by the field bytes, in this exact
order:

1. `sandbox_id`            (UTF-8)
2. `commit_hash`           (UTF-8, the 40-hex string)
3. `content_hash`          (UTF-8, the 64-hex string)
4. `key_id`                (UTF-8, the 64-hex fingerprint)
5. `issued_at_unix_nano`   (the 8 raw big-endian bytes of `IssuedAt.UTC().UnixNano()`, an int64 — **not** a decimal string, **not** the RFC3339 form)

The host signs this payload with Ed25519. Binding `key_id` into the
payload rules out algorithm/key confusion across rotations. Length
prefixing rules out field-boundary collisions.

### 3.4 Verification

`Verify` MUST, in order:
1. `Attestation.Validate()` (structural shape).
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

## 6. Atomic land import — *MGIT-11.8.5*

Land is all-or-nothing (squash semantics, FR-2.x): every commit verifies
and imports inside one serializable transaction, or none do. Success
fast-forwards the task branch append-only; the host records its own
receive-time alongside the advisory guest timestamp (SEC-11).
