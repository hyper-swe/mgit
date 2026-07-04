# Feature → e2e coverage matrix

What each mgit capability is proven by, end to end. "e2e" means a real
installed-style binary (or real server process) driven by
`scripts/e2e/*` — the jobs in `.github/workflows/e2e.yml`, which gate every
release (`release.yml` `needs: e2e`) and run locally via `make e2e`.
"unit" means covered by `go test` (90/85 coverage bar) but not proven through
a real binary/process. Update this file whenever an e2e job or script changes
(MGIT-53); a claim in the README with no e2e row here is a gap, not an
oversight to paper over.

Legend: **e2e** proven end-to-end · **gated** e2e exists but needs
environment (skips in hosted CI) · **unit** unit/integration tests only ·
**—** uncovered.

| Capability / claim | Proof | Where |
|---|---|---|
| `init` over an existing git repo | e2e | core_loop.sh |
| `worktree add/list/remove` (CLI) | e2e | core_loop.sh |
| task-tagged `commit` in a worktree (auto-inherited task) | e2e | core_loop.sh |
| `add`, `status`, `log`, `verify`, `audit` | e2e | core_loop.sh |
| `squash --to-git \| git apply` round-trip | e2e | core_loop.sh |
| **Course-correction loop** (content-restoring rollback → fork → `restore --all` checkpoint recovery → materializing cherry-pick → squash, append-only) | e2e | course_correction.sh |
| `checkout -b` / `cherry-pick` / `rollback` / `restore [--all] --commit` | e2e | course_correction.sh (content-restoring semantics since MGIT-54/55) |
| Install channels produce working binaries (`go install`, release archive incl. `mgit-sandboxd`) | e2e | e2e.yml install-channels matrix |
| Daemon-less honest posture (`mgit work` open, no shims, truthful CLAUDE.md, `Containment:` line) | e2e | daemonless_posture.sh |
| `mgit run` fails closed with install pointer when no daemon | e2e | daemonless_posture.sh |
| MCP: full tool surface registered, no placeholders | e2e | mcpdrive |
| MCP: every registered tool driven through real stdio (`serve --mcp-only`) | e2e | mcpdrive |
| MCP: hostile input → structured tool errors | e2e + unit | mcpdrive; internal/mcp tests |
| REST: documented `/api/v1` routes over a real server process | e2e | rest_posture.sh |
| REST: loopback-only bind, unauthenticated same-user model (MGIT-51) | e2e + unit | rest_posture.sh; serve tests |
| Serve/CLI lock coexistence as two real processes (MGIT-46) | e2e | rest_posture.sh |
| Sandbox: launch → `run` in guest → verified `land` | gated | sandbox_posture.sh (needs daemon + KVM/entitlement + guest image; live pass per platform mandated by docs/release/RELEASE-CHECKLIST.md) |
| Sandbox: egress allowlist, SEC-03 quarantine, attestation | gated + unit | backend e2e (env-gated) + internal tests; live validation logged per release |
| ADR-008 auto-resync / pinned fork-base | unit | internal/service sync tests |
| `show`, `branch` (CLI), `config`, `diff`, `export`, `merge`, `restore`, `gc`, `import`, `docs generate` | unit | package tests |
| `work --base`, `.mgit/seed-include` | unit | package tests |
| Multi-agent parallel worktrees (N tasks side by side) | unit | worktree service tests |
| Sandbox lifecycle verbs (`exec`, `shell`, `grants`, `image`) | unit + gated | daemon tests; backend e2e |
| Windows core loop (no sandbox) | — | uncovered: all e2e jobs are ubuntu; unit suite not run on a Windows runner |
| Homebrew install channel | — | uncovered in CI (tap lives in a separate repo); verified manually at release |

Refs: MGIT-48, MGIT-53. Companion: [MCP-PARITY.md](MCP-PARITY.md) (surface
parity), [release/RELEASE-CHECKLIST.md](release/RELEASE-CHECKLIST.md) (live
sandbox passes).
