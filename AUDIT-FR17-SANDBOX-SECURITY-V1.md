# FR-17 MicroVM Sandbox (ADR-005) — Critical Security Audit V1

**Audit Date:** June 12, 2026
**Auditor:** Adversarial Security Review (red-team)
**Scope:** ADR-005 design, attacked as a security boundary — not a standards checklist
**Method:** Assume the attacker has *already won inside the guest* (the design's own premise: the guest is where the npm malware runs). Goal of the attacker: escape to host, corrupt the audit record, exfiltrate, or weaken the boundary for future tasks.
**Verdict:** **NOT YET SECURE FOR IMPLEMENTATION — 4 Critical / 5 High / 3 Medium. The 4 Criticals are design-level (not bugs) and are remediated in ADR-005 this cycle.**

---

## 1. Executive Summary

The hardware boundary is sound. The weaknesses are not in the VM — they are in the **four places where host and guest meet**: the shared worktree mount, the vsock land/attestation channel, the egress proxy, and the policy/config files. Every Critical below is an attacker using a host↔guest seam to defeat a property the ADR claims. The recurring root cause: **trusting data or attestations that originate on the guest side of the boundary.**

| Severity | Count | Status |
|----------|-------|--------|
| Critical | 4 | Remediated in ADR-005 (design changes) |
| High | 5 | Tracked → adoption criteria |
| Medium | 3 | Tracked |

---

## 2. Critical Findings

### SEC-01 — Guest-rooted attestation is forgeable (the guest cannot vouch for itself)

*Claim attacked:* `require_sandbox` / commit attestation (F-02 remediation) guarantees landed commits were produced in a real sandbox.
*Attack:* The attestation token is issued by `mgit-guest`, which runs **inside** the untrusted guest. A compromised guest owns that process and its keys, so it can mint valid attestations for any commit — including malicious or externally-injected ones. An attestation signed by the thing being attested is worthless: the guest is precisely the threat actor in this model.
*Impact:* Defeats the entire integrity story. A compromised toolchain can forge provenance.
*Remediation (applied):* Attestation is **host-anchored**. `mgit-sandboxd` (host side) observes each commit as it crosses vsock and issues the attestation itself, keyed by host-held material the guest never sees. The guest cannot produce a valid attestation; it can only *transmit objects the host then attests*. Guest-side `mgit-guest` is demoted to a transport, holds no signing key.

### SEC-02 — Self-weakening configuration (guest disables its own containment)

*Claim attacked:* `require_sandbox`, network policy, image pinning are enforced.
*Attack:* The ADR placed policy in `.mgit/sandbox/policy.json` and `images.lock`. If any enforcement input lives inside the repo/worktree — which **is** the one writable mount — the guest (or the agent it manipulates) edits `policy.json` to set `require_sandbox=false`, rewrites `netpolicy.json` to `open`, or repoints an image digest, then those changes land and weaken the *next* task. Containment that can be turned off from inside the container is not containment.
*Impact:* One compromised task lowers the boundary for all subsequent tasks; persistent, repo-propagating weakening.
*Remediation (applied):* All enforcement inputs (`require_sandbox`, network policy, image lock, sensitive-path list, resource caps) are **host-only state outside the worktree**, never mounted into the guest, never committable repo files. They live under a host config root (e.g. `~/.mgit/host/<repo-id>/`). A repo *may* ship suggested defaults, but they take effect only after explicit host-side adoption, and the effective policy is recorded in the audit log. `ErrSensitivePathModified` already blocks guest edits to host-trusted paths; this extends the protected set to all enforcement config.

### SEC-03 — Shared object store leaks into the guest (conflict with ADR-004)

*Claim attacked:* Quarantine — "the guest sees only the worktree; the parent repo and `.mgit/objects/` are never mounted."
*Attack:* ADR-004 states all worktrees **share** `.mgit/objects/` and `.mgit/refs/`. A normal git worktree's `.git` file points back into that shared store. If the sandbox naively mounts "the worktree," it mounts a `.git`/`.mgit` link that resolves to the shared object store and index DB — giving the guest read access to *other tasks'* objects and write access to the shared index. Quarantine is silently broken by inheriting ADR-004's sharing.
*Impact:* Cross-task data exposure (T6), direct write to the shared store bypassing the verified land path (T5).
*Remediation (applied):* The guest mount is the **working tree files only**. The guest's `.git` is rebound to a **private, sandbox-local object store**; the host shared store, index DB, refs, and parent `.mgit` are never resolvable from inside the guest. Land is the *only* path from the private store to the shared store, and it is the verifying, append-only door. Explicit reconciliation of this tension is added to the ADR and flagged for ADR-004 cross-reference.

### SEC-04 — Egress allowlist is defeatable by 2026 network reality

*Claim attacked:* `allowlist` mode confines egress to approved hosts (T3 exfiltration, T9 lateral movement).
*Attack:* Host/SNI-based allowlisting assumes the host is visible and truthful. In 2026 it often is not: (a) **Encrypted ClientHello (ECH)** hides SNI entirely, so an SNI allowlist sees nothing to match; (b) **domain fronting** presents an allowed SNI while the real Host header targets elsewhere; (c) **raw-IP connections** skip DNS/SNI; (d) **QUIC/UDP/HTTP-3** and arbitrary outbound UDP bypass an HTTP/TLS-only proxy; (e) **DNS tunnelling** exfiltrates via subdomain labels through the host resolver even with zero TCP allowed.
*Impact:* The proxy provides a false sense of containment; a motivated payload exfiltrates source or reaches metadata endpoints despite `allowlist`.
*Remediation (applied):* Egress enforcement is specified as **default-deny at the IP/flow layer, not SNI parsing**: the guest gets no direct route; the host proxy permits only connections whose resolved destination IP is on the allowlist (DNS pinned host-side, resolution restricted to allowlisted names), drops all UDP except DNS to the host resolver, blocks QUIC, treats ECH/unresolvable-SNI as deny, denies RFC1918 + link-local + metadata IPs unconditionally, and rate-limits + logs DNS to throttle tunnelling. `open` mode is documented as explicitly disabling T3/T9 — a user-accepted risk, never a default.

---

## 3. High Findings

**SEC-05 — Guest-controlled remedy string social-engineers capability grants.**
The `MGIT-EGRESS-DENIED ... remedy=<...>` string is guest-sourced; if the host grant prompt is built from it, a payload crafts a remedy requesting broad egress and relies on the agent to relay it. *Fix:* the host derives the grant request **solely from the observed denied connection** (actual dest IP/port), never from guest text; prompts always show real destination + requesting task; no "allow all" option. (Tracked; partially covered by Residual-Risk #5 in ADR.)

**SEC-06 — Land-path TOCTOU (verify-then-import on re-fetched bytes).**
If verification hashes one fetch and import writes a second fetch, a guest serves clean bytes to the verifier and malicious bytes to the importer. *Fix:* verify the **exact buffer that is imported** (hash-on-write, single read), specified in the vsock land protocol.

**SEC-07 — DNS as covert/exfil channel.**
Even with TCP fully denied, host-side DNS resolution lets the guest encode data in query labels. *Fix:* restrict resolvable names to the allowlist, cap query rate/volume, log NXDOMAIN bursts.

**SEC-08 — Warm-pool / snapshot contamination.**
Snapshot-restore for <200 ms starts must originate from a **clean base-image snapshot only** — never a snapshot of a used guest (would carry prior-task state/payload). Reclaimed/ballooned pages returned to host must be zeroed. *Fix:* snapshot provenance rule + page zeroing in FR-17.

**SEC-09 — Host loopback exposure + host DoS.**
Guest→host port *publish* (dev server) must be strictly one-way; the guest must not reach host loopback services (e.g., a host DB on `127.0.0.1:5432`). Also a global cap on concurrent sandboxes is needed — an agent loop spawning tasks could exhaust host RAM/CPU even with per-VM caps. *Fix:* one-way publish only; global concurrency + total-memory ceiling in `mgit-sandboxd`.

---

## 4. Medium Findings

**SEC-10 — vsock peer binding.** Each guest's control channel must be bound to its CID; one guest must not address another's land/attestation channel. Specify CID authentication in the vsock protocol.

**SEC-11 — Commit author/timestamp forgery.** Task-ID mismatch is already caught (`ErrTaskMismatch`), but guest-forged author/timestamp degrade audit fidelity. Record host-observed receive-time alongside guest timestamp; treat guest author as advisory.

**SEC-12 — Image signature verification.** Digest pinning detects tampering only relative to the lock; signed images verified at boot (cosign-class) detect a poisoned lock entry. Requires package approval (tracked as F-10).

---

## 5. What Survived the Red-Team

| Property | Verdict |
|----------|---------|
| Hardware isolation boundary (kernel/FS/persistence) | ✅ holds |
| Append-only event-sourced audit (post F-01 fix) | ✅ holds |
| Dual-hash verification *once anchored host-side* (SEC-06 closes the gap) | ✅ holds |
| Ephemeral COW teardown erases host residue | ✅ holds |
| ssh keys never enter guest (agent forwarding) | ✅ holds |
| Reduced-isolation fallback requires explicit, audited acknowledgment | ✅ holds |

---

## 6. Disposition

The 4 Criticals share one lesson: **never trust the guest side of a seam.** Each remediation moves a trust decision (attestation, policy, object store, egress matching) fully onto the host. With those applied, the design's security posture matches its claims. Highs/Mediums are added to ADR-005 adoption criteria and block "Accepted."

**Re-audit trigger:** after the vsock land/attestation protocol is specified in FR-17, security audit V2 verifies SEC-05..SEC-12 and fuzzes the land parser.
