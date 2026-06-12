# FR-17 MicroVM Sandbox (ADR-005) — Standards Audit V1

**Audit Date:** June 12, 2026
**Auditor:** Safety-Critical Systems Review
**Scope:** ADR-005 (proposed FR-17, `mgit sandbox`) design audit — pre-implementation
**Target Standards:** DO-178C Level A, IEC 62304, NASA-STD-8739.8, MIL-STD-498, OWASP ASVS Level 2, plus mgit internal laws (CLAUDE.md)
**Verdict:** **CONDITIONALLY APPROVED — 3 P1 / 6 P2 / 3 P3 findings; P1s remediated in ADR-005 during this audit cycle**

---

## 1. Executive Summary

ADR-005 is structurally strong on the dimensions these standards care most about: hardware isolation boundary, append-only audit intent, dual-hash verified import path, default-deny network, supply-chain pinning, and per-commit environment provenance. The provenance model (commit → sandbox → image digest → network posture) *exceeds* what the existing FR set records and strengthens the DO-178C CM story.

However, the audit found one violation of mgit's own append-only law, one silent-bypass hazard, and several places where the design asserts security properties without specifying the mechanism a verifier would test.

| Severity | Count | Status |
|----------|-------|--------|
| P0 (Blocker) | 0 | — |
| P1 (Critical) | 3 | **Remediated in ADR-005 this cycle** |
| P2 (Important) | 6 | Tracked → adoption criteria |
| P3 (Minor) | 3 | Tracked → adoption criteria |

---

## 2. Findings

### P1 — Critical

**F-01: `sandbox_sessions` schema violates the append-only law.**
*Standard:* mgit SQL Rule 5, Append-Only Enforcement (CLAUDE.md); DO-178C §7.2 CM records integrity.
*Finding:* The proposed table contained `ended_at` and `end_reason` columns on the session row — populating them at teardown requires `UPDATE`, which is forbidden on audit tables. The same law mgit enforces on `task_commits` was broken by the new design.
*Remediation (applied):* Replaced with an event-sourced `sandbox_events` table (created/suspended/resumed/policy_granted/landed/destroyed/...). Session state is derived from the latest event; no row is ever updated or deleted.

**F-02: Cooperative routing permits silent unsandboxed execution.**
*Standard:* IEC 62304 §7 risk control (hazard: containment silently absent); NASA-STD-8739.8 hazard analysis.
*Finding:* For harnesses without enforced hooks (Codex, Cursor — AGENTS.md/PATH-shim adapters are *cooperative*), a command can run on the host with no containment and no error. Nullable `sandbox_id` makes this detectable after the fact, but nothing prevents unsandboxed commits from landing.
*Remediation (applied):* Added `require_sandbox` policy (default **on** for safety-critical profiles): `land` refuses commits lacking a guest **attestation** — a per-commit token issued by `mgit-guest` over vsock at commit time, proving the commit was created inside an attested session. Unsandboxed work is then a visible, blocked state, not a silent gap.

**F-03: Land path parses hostile input with no validation specification.**
*Standard:* OWASP ASVS V5 (input validation); CLAUDE.md Security Mindset ("treat all input as hostile") — the guest is *the* hostile party in this design (T1: it ran the npm malware).
*Finding:* `sandbox land` ingests guest-supplied commit objects, tree entries, and metadata over vsock. The ADR specified hash verification but no protocol-level validation: message schema, size ceilings, object-count limits (zip-bomb class), path traversal in tree entry names, control characters in messages destined for the audit log.
*Remediation (applied):* Land-path hardening requirements added to ADR-005 (schema-validated vsock protocol, size/count ceilings, tree-path canonicalization, reject non-canonical encodings) and a full protocol spec gated in adoption criteria.

### P2 — Important

**F-04: DO-178C tool qualification position unstated.**
ADR-003 scopes mgit's DO-178C posture, but ADR-005 introduces new tool components (`mgit-sandboxd`, VMM, guest agent) without stating their DO-330 position. As CM/verification tooling, mgit sits in tool criteria 3 (TQL-5 at Level A); the hypervisor/VMM is COTS requiring an assessment record. *Action:* extend ADR-003 or add a tool-qualification section to FR-17.

**F-05: No SOUP/COTS register for sandbox components (IEC 62304 §8.1.2).**
APPROVED-PACKAGES.md covers Go dependencies only. Rootfs images, guest toolchains, and the VMM are SOUP: each needs identification, version pinning (digest ✓), and known-anomaly review. *Action:* `SANDBOX-IMAGES.md` manifest paralleling APPROVED-PACKAGES.md, referenced from images.lock.

**F-06: No verification independence on the land path (DO-178C Level A §6.2).**
The same binary that imports commits also verifies them. *Action:* specify an independent re-verification mode (`mgit verify --independent`, clean-install binary or separate verifier) for compliance workflows.

**F-07: No off-nominal/fault-injection test categories (NASA-STD-8739.8).**
Acceptance criteria covered performance, not faults. *Action:* required test categories — VM killed mid-exec, vsock dropped mid-land (atomicity must hold), virtiofs corruption, snapshot-restore integrity, TTL expiry during land, attestation replay.

**F-08: `mgit-sandboxd` local IPC lacks an authentication spec (ASVS V4).**
Any local process could otherwise instruct the daemon to boot VMs or grant capabilities. *Action:* peer-credential verification (same-UID check via SO_PEERCRED/equivalent) + socket permissions, specified in FR-17.

**F-09: Audit-log injection via guest-controlled strings (ASVS V7).**
Egress hostnames, commit messages, and event detail originate in the guest and are written to append-only tables — append-only makes *corrupted* entries permanent. *Action:* sanitize/encode guest-sourced strings before insertion; length caps; parameterized inserts (already law).

### P3 — Minor

**F-10:** Images are digest-pinned but not signature-verified; recommend signed images verified at boot (supply chain, ASVS V10). Signing tooling requires package approval first.
**F-11:** vsock control/land protocol needs an interface specification document (MIL-STD-498 IDD-equivalent); FR-17 itself not yet in REQUIREMENTS.md (already tracked in adoption criteria).
**F-12:** Image update/change-control process for `images.lock` undefined — images are configuration items (DO-178C §7.1); define review + re-baseline procedure.

---

## 3. What Passed

| Check | Result |
|-------|--------|
| Append-only `task_commits` untouched by design; land appends only | ✅ |
| Dual-hash (ADR-002) re-verification at trust boundary | ✅ |
| Default-deny network posture (allowlist default, `open` is opt-in) | ✅ |
| Per-commit environment provenance (sandbox/image/network) — exceeds current FR set | ✅ |
| Fail-closed permission hook (sandbox down → prompts resume) | ✅ |
| No secrets in guest; ssh keys never leave host (agent forwarding) | ✅ |
| Capability grants audited, scoped to sandbox lifetime | ✅ |
| CGO containment via helper binary; core stays pure-Go | ✅ |
| Reduced-isolation fallback requires explicit acknowledgment, recorded in audit | ✅ |
| Sentinel error naming, JSON snake_case, ISO-8601 UTC conventions | ✅ |

---

## 4. Disposition

ADR-005 remains **Proposed**. P1 remediations applied in this cycle (see ADR-005 revision history). P2/P3 actions added to ADR-005 adoption criteria; none block continued design work, all block "Accepted" status.

**Re-audit trigger:** when FR-17 is drafted into REQUIREMENTS.md, audit V2 verifies F-04..F-12 dispositions and FR/test traceability.
