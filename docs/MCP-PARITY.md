# mgit surface parity: CLI × MCP × REST

The audit MGIT-50 requires the MCP surface to be GA-quality and its gaps
explicit. This matrix maps each mgit capability across the three surfaces. The
MCP column is generated-adjacent: the tool set is enumerated from the live
registered tools (`Server.ToolDocs`), and a drift guard
(`internal/mcp.TestToolDocs_CoversRegisteredSurface`) fails if it changes
without this doc and the `docs generate` reference being updated.

Legend: ✓ full · ~ partial (see notes) · ✗ not offered.

| Capability            | CLI | MCP | REST | Notes |
|-----------------------|:---:|:---:|:----:|-------|
| commit                | ✓  | ✓  | ✓   | `mgit_commit` / `POST /commits` |
| log                   | ✓  | ✓  | ✓   | `mgit_log` / `GET /commits` |
| status                | ✓  | ✓  | ✗   | `mgit_status` returns file status JSON (MGIT-50) |
| show                  | ✓  | ✓  | ✓   | `mgit_show` / `GET /commits/:id` |
| branch                | ✓  | ✓  | ✓   | create/list; `GET/POST /branches` |
| verify                | ✓  | ✓  | ✓   | `mgit_verify` / `GET /verify` |
| diff                  | ✓  | ✓  | ✗   | `mgit_diff` (commit pair or task) (MGIT-50) |
| export                | ✓  | ✓  | ~   | `mgit_export` JSON; REST `GET /tasks/:id/commits` |
| audit                 | ✓  | ✓  | ✗   | `mgit_audit` returns real trail JSON (MGIT-50) |
| config                | ✓  | ✓  | ✗   | `mgit_config` get/set (MGIT-50) |
| squash                | ✓  | ~  | ~   | see **Documented gaps** |
| rollback              | ✓  | ✓  | ✓   | `mgit_rollback` / `POST /rollback` |
| worktree add/list/remove | ✓ | ✓ | ✗ | `mgit_worktree_*` (MGIT-45) |
| checkout / cherry-pick / merge | ✓ | ✗ | ✗ | history-editing verbs, CLI-only by design |
| gc / restore / import / bundle | ✓ | ✗ | ✗ | maintenance verbs, CLI-only |
| sandbox sync (FR-17.40) | ✓ | ✓ | ✗ | `mgit_sandbox_sync` — re-stage the host worktree into a running guest; `dry_run` returns the conflict classification (MGIT-76) |
| sandbox egress policy (FR-17.8) | ✓ | ✓ | ✗ | `mgit sandbox policy set/revoke/show` / `mgit_sandbox_policy` (MGIT-72) |
| sandbox artifact export | ✓  | ✓  | ✗   | `mgit sandbox export` / `mgit_sandbox_export` (MGIT-73) |
| run / sandbox lifecycle (FR-17) | ✓ | ✗ | ✗ | launch/exec/shell/land/grants/image: CLI-only (see **Documented gaps**) |

## Documented MCP gaps

These are intentional or deferred; recorded here so an agent is not surprised
(README / agent docs link this).

- **squash export/promote (`--to-git`, `--to-main`)** is CLI-only. `mgit_squash`
  produces the task-isolated squash artifact on `task/<id>`; exporting a git
  patch or promoting to `main` is a deliberate landing action kept on the CLI.
  Over MCP, squash then read the result and land via the CLI
  (`mgit squash --task <id> --to-git | git apply`).
- **`mgit_worktree_add` has no `--base`.** It forks from the auto-resynced local
  base (ADR-008); pinning an explicit base is CLI-only for now
  (`mgit work --base <ref>`).
- **`mgit_diff` has no `--stat` / format switch.** It returns a unified diff.
- **History-editing and maintenance verbs are CLI-only** — they are either
  destructive (checkout/cherry-pick/merge) or local maintenance
  (gc/restore/import/bundle).
- **The sandbox LIFECYCLE verbs are CLI-only** (`launch`, `exec`, `shell`,
  `land`, `grants`/`grant`, `image`, `list`/`status`/`remove`). Provisioning a
  microVM, approving a capability escalation, and landing guest commits are
  operator decisions with host-side consequences; an agent reaches its sandbox
  by having its shell routed through `mgit run`, not by driving the lifecycle.
  `mgit_sandbox_sync` is the deliberate exception: it is the one sandbox
  operation an agent needs BETWEEN rounds, it grants no new authority (the
  daemon re-stages through the same host-side invariants a launch enforces),
  and its `dry_run` form is the only way to learn which paths diverged without
  running a command in the guest and being refused. Refs: MGIT-76, ADR-011
- **Per-sandbox resource caps (`--cpus`/`--memory-mb`/`--disk-quota-mb`) are
  CLI-only** — they are launch parameters, and launch is not on MCP. The
  decision was re-examined for R-H212 (an agent whose build died against an
  invisible memory ceiling rewrote a customer's bundler config to fit it) and
  the flags stayed off MCP for two reasons. First, sizing a guest *is* the
  lifecycle decision: it commits host RAM and CPU, so an agent that could
  declare its own size could walk itself up to the per-sandbox maximum
  unattended — the authority the lifecycle exclusion exists to withhold.
  Second, the agent-facing half of that problem is not authority but
  VISIBILITY, and that is served without a new tool: `mgit sandbox status`
  reports the effective caps, the generated CLAUDE.md states them, and a
  signal-death exit prints the cap it ran under. An agent can therefore see
  its ceiling and report it — which is the correct action — while raising it
  remains an operator's (or the launching lane's) call on the CLI.
  Refs: R-H212, NFR-17.5

## Live egress policy is on MCP on purpose (MGIT-72)

`mgit_sandbox_policy` is the second sandbox verb offered over MCP (after
`mgit_sandbox_sync`), and for the same reason: an **agent is its intended
caller**, not an operator. The
sequence it exists for is one an agent runs unattended:

1. grant package-registry egress, `npm install` / `pip install`,
2. revoke it,
3. run the untrusted build and tests with the network closed.

Before this, the only way to revoke was to relaunch the sandbox — which
destroys the environment step 1 just provisioned — so callers held egress open
for the whole run. Leaving the verb off MCP would have left the agent that
needs it with nothing to call, which is why it is here while the lifecycle
verbs are not.

Two properties of the tool are worth knowing before you call it:

- **`action: "revoke"` TERMINATES established connections** unless you pass
  `drain: true`. That is the opposite of firewall convention and it is
  deliberate — a draining connection is the exfiltration channel you just
  revoked, and a hostile guest chooses how long it lives. The reasoning is in
  ADR-012.
- **`action: "set"` requires at least one `allow` entry.** An empty `set` and a
  `revoke` are the same operation underneath, so an empty `set` is refused
  rather than silently revoking everything.

Every mutation is written to the append-only sandbox audit trail with the task
binding, the resulting policy, and how many established flows it terminated.
`action: "show"` reports the policy **in force**, which after a mutation is not
the launch-time policy on `mgit sandbox status`.

- **`mgit_sandbox_export` is on MCP for the same reason as the other two** (MGIT-73):
  the caller is the agent that just built the artifact. It is registered on every
  server so an agent can discover it; where no sandbox daemon is wired it fails
  closed naming that reason, never a fake success. Both paths are host-named and
  validated at the boundary before the daemon sees them; the containment checks
  (symlink/hardlink escapes, traversal, collision, size and file-count ceilings)
  and the append-only audit record live host-side in the daemon, so the MCP and
  CLI routes have identical semantics.

## REST scope (decision record, MGIT-52)

The REST column's gaps are a **decision, not drift**. REST is formally scoped
as a minimal same-host integration surface: health, commits (create/get/list),
task commits, branches (create/list), squash artifact, rollback, and verify.
Everything else (worktrees, diff/status/audit/config, export formats, sandbox)
is served by the CLI (humans, scripts) and MCP (agents).

Rationale:

- **Trust model bounds the surface.** REST always binds `127.0.0.1` and is
  unauthenticated (NFR-5.11 as amended by MGIT-51): its callers are same-user
  local processes, which could equally invoke the CLI. A broader REST surface
  adds parity-maintenance load without adding capability.
- **Three surfaces at full parity is permanent drift risk.** MCP has a drift
  guard tied to the live tool registry; REST does not, so its scope is kept
  small and stable instead.
- **Expansion has prerequisites.** Any route beyond this list, or any
  non-localhost exposure, first requires a named consumer and the
  authentication lifecycle reinstated per NFR-5.11's superseded spec
  (MGIT-51).

## GA-quality guarantees (all MCP tools)

- **Same service layer as the CLI.** Handlers contain no business logic; they
  delegate to the same `service.*` types, so semantics, validation, and the
  append-only audit guarantee are identical.
- **Hostile input is rejected.** Every tool validates its arguments before the
  service call: task ids against the MGIT-41 grammar (an allowlist that rejects
  control chars, path separators, shell/SQL metacharacters), worktree paths
  against traversal / control chars / NUL / oversize, free text against
  NUL / oversize. See `internal/mcp/validate.go`.
- **Structured errors.** Failures come back as MCP tool errors (`IsError`)
  carrying the service's sentinel-wrapped message, never a raw internal dump or
  a fake-success placeholder.
- **Tested through the real server.** `internal/mcp/ga_inprocess_test.go` drives
  an in-process MCP client through the real dispatch (initialize → list → call),
  covering happy, error, boundary, and hostile-input paths.

Refs: MGIT-50, MGIT-45, MGIT-41, MGIT-20. Companion:
[E2E-MATRIX.md](E2E-MATRIX.md) maps every capability (all surfaces) to its
end-to-end proof.
