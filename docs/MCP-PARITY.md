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
| run / sandbox (FR-17) | ✓ | ✗ | ✗ | containment lifecycle, CLI-only (needs the daemon) |

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
- **History-editing, maintenance, and sandbox verbs are CLI-only** — they are
  either destructive (checkout/cherry-pick/merge), local maintenance
  (gc/restore/import/bundle), or require the sandbox daemon (run/sandbox).

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

Refs: MGIT-50, MGIT-45, MGIT-41, MGIT-20
