# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Documentation

- **README and agent docs now match shipped reality.** Added an "Enable the sandbox" section (install `mgit-sandboxd` per channel, guest image, platform prerequisites) and stated in Quick start that `--sandbox` requires it while everything else works without it, with the no-sandbox integration path (`squash --to-git | git apply`). Corrected two wrong commands the docs advertised: the MCP server is `mgit serve --mcp-only` (there is no `mgit mcp`), and the REST API is `127.0.0.1`-only covering a subset of operations (dropped the unimplemented `mgit token generate` bearer-auth claim). The agent working-discipline skill gains pitfalls for the daemon-less posture, sandbox-only landing, the serve lock, and the now-working MCP worktree tools. (MGIT-49)

### Added

- **Install-channel + posture e2e in CI (release gates).** New jobs exercise what a real user gets — an installed binary, no repo checkout — across both postures: the core loop over an installed `mgit` (`squash --to-git | git apply` round-trip included), the daemon-less honest degraded mode, the full MCP tool surface driven through a real stdio client, and a virtualization-gated sandbox pass. A regression like "mgit-sandboxd missing from the archives" or "an MCP tool returns placeholder" now fails CI before users see it. Run locally with `make e2e`. (MGIT-48)

### Added

- **The flagship claims now have e2e proof (release gates).** New always-on e2e legs: the course-correction loop end to end (micro-commits with a wrong step → append-only rollback → fork → salvage from a checkpoint → squash, with the abandoned attempt asserted to survive in log + audit); the REST surface driven route-by-route against a real `mgit serve` process; serve/CLI **lock coexistence proven as two real processes** (CLI commit/status/worktree complete promptly while serve runs); and the MCP e2e now **calls every registered tool** through the real stdio server (previously 11 of 15 were only registration-checked). A feature→e2e coverage matrix lives at [docs/E2E-MATRIX.md](docs/E2E-MATRIX.md) so gaps stay visible (Windows and brew-in-CI are listed as uncovered rather than papered over). `core_loop.sh` now actually asserts `mgit status`, which its header claimed. (MGIT-53)

### Fixed (documentation honesty)

- **The README's course-correction steps now match verified CLI behavior.** e2e-proving the loop surfaced that `mgit checkout <hash>` does not exist (checkout is branch-only), `rollback` reverts the owning task as an append-only record (and does not restore content), and `cherry-pick` records provenance rather than materializing bytes — salvage is `mgit restore <file> --commit <hash>`. The diagram, steps, and command tables now describe the loop that actually works; the underlying product gaps are tracked (MGIT-54 content-restoring rollback/cherry-pick, MGIT-55 whole-tree checkpoint recovery, MGIT-56 status-time auto-sync emptying a task's first-commit diff). (MGIT-53)

### Changed

- **Dead REST auth code removed; trust model made explicit.** The unwired `TokenStore`/Bearer middleware (a security control that was present but never enforced) is deleted, and the REST API's real model is now stated everywhere: it always binds `127.0.0.1` (hardcoded; the former `api.bind_address` config key is gone) and is unauthenticated by design — its callers are same-user local processes, the same trust as running the CLI. NFR-5.11 amended with the decision; the token-lifecycle spec is retained there for reinstatement if remote access is ever offered. The `api.http_port` config key now actually works: `mgit serve` uses it when `--port` is not passed. (MGIT-51)
- **REST formally scoped as a minimal same-host integration surface** (health, commits, task commits, branches, squash artifact, rollback, verify) — the parity matrix's REST gaps are now a recorded decision with rationale, not drift. Expansion requires a named consumer plus the NFR-5.11 auth lifecycle. See [docs/MCP-PARITY.md](docs/MCP-PARITY.md). (MGIT-52)

### Fixed

- **`mgit serve` shuts down when its MCP stdio client disconnects** (stdin EOF) instead of blocking until a signal — a stdio server's client connection is its lifecycle. (MGIT-48)
- **`mgit work` on a machine without the sandbox no longer misleads the agent.** Previously it installed PATH shims that routed every command through fail-closed `mgit run` (so the agent "couldn't even echo") and wrote a CLAUDE.md claiming "your shell already routes through `mgit run`" — even with no sandbox daemon present. Now the wiring is containment-aware: with no `--sandbox` (honest-open) no routing shims or hook are installed and CLAUDE.md states plainly that commands run on the host and how to enable containment; with `--sandbox` (containment requested) the fail-closed routing stays, and the security invariant holds — mgit never silently bypasses a requested sandbox. `mgit work` also prints a single parseable `Containment: …` status line. (MGIT-47)
- **A long-running `mgit serve` no longer starves the CLI.** The server used to hold the exclusive repo lock for its entire lifetime, so with `mgit serve` running (e.g. an agent's MCP server), every CLI command on the same repo failed after a 30-second wait (`another mgit process is running`). The server now acquires the lock **per operation** — the same scope a CLI command holds it — so a driving agent over MCP/REST and a human on the CLI can work the same repo concurrently. A contended-lock error now also names the holding command, not just its PID. See [ADR-009](docs/adr/009-per-operation-locking.md). (MGIT-46)
- **The MCP surface is now GA-quality across the board.** The last stubbed tools — `mgit_status`, `mgit_diff`, `mgit_audit`, `mgit_config` — return real data through the same service layer as the CLI instead of canned placeholders (`"no changes"`, `"working tree clean"`, …). Every tool now validates its arguments as hostile input before touching a service (task ids against the MGIT-41 grammar; worktree paths against traversal / control chars / NUL / oversize; free text against NUL / oversize) and returns structured tool errors. The generated MCP reference (`mgit docs generate`) is derived from the live registered tool set, so it cannot drift; a capability parity matrix (CLI × MCP × REST, with documented gaps) is at [docs/MCP-PARITY.md](docs/MCP-PARITY.md). (MGIT-50)
- **The MCP worktree tools now work.** `mgit_worktree_add`, `mgit_worktree_list`, and `mgit_worktree_remove` previously returned a fake-success placeholder (`"not yet available (Wave 11)"`); a driving agent that relied on them got nothing. They now delegate to the same `WorktreeService` the CLI uses — `mgit_worktree_add` materializes a real task-bound worktree with the ADR-008 pinned fork-base, and the tools return structured JSON / errors. (MGIT-45)
- **The sandbox daemon `mgit-sandboxd` is now shipped by every host channel.** Previously the release built only `mgit`, so Homebrew / `go install` / release-archive users never received the daemon and the microVM containment pillar was uninstallable — an external trial concluded mgit was unusable as a working substrate. Release archives (Linux any arch, macOS arm64) now contain **both** binaries side by side; the macOS daemon is built with CGO and code-signed with the `com.apple.security.virtualization` entitlement on an Apple Silicon runner; `go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest` is documented (with the macOS signing caveat). `mgit-guest` continues to ship inside the guest image, not on host `PATH`. See [docs/INSTALL-SANDBOX.md](docs/INSTALL-SANDBOX.md). (MGIT-44)

## [0.2.1-beta] - 2026-06-29

### Fixed

- **`mgit branch --delete` left a stale branch row in the index**, so `mgit worktree add` for the same task later failed with `branch already exists` and no clean recovery (gc/prune didn't help). Delete now clears **both** the go-git ref and the SQLite index in one operation — and clears a stale row even when the ref is already gone, so an already-stranded task recovers. Branch creation also **self-heals** a stale row and is now atomic across both stores (a failed index write no longer leaves a partial ref behind). (MGIT-42)

### Documentation

- Landing-page README reworked to lead with the benefit in plain language (run agents safely; keep a clean, reviewable history), with an independent-trial testimonial and a two-minute try-it CTA — the deep technical sections remain below.
- The agent skill gains a **"Common pitfalls (and the fix)"** section so an agent working through mgit avoids the known friction (worktrees aren't git repos, build artifacts need `.mgit/seed-include`, `--task-id` flag, etc.) up front.

## [0.2.0-beta] - 2026-06-26

First beta to ship the full agent-substrate **and** containment product. `main`
had run far ahead of `v0.1.0-beta` (the sandbox, `mgit work`, worktree
materialization, and git coexistence were all unreleased); this release makes
that work installable.

### Added

- **Per-task microVM containment (FR-17)** — untrusted installs/builds/tests run in a disposable, hardware-isolated microVM, not on the host. Backends: firecracker (Linux/KVM, live-validated) and Apple Virtualization.framework (macOS arm64, live-validated). Default-deny per-task egress allowlist enforced host-side; verified land (dual-hash + task binding + host-anchored attestation) through an airlock; per-sandbox quarantined `.mgit` with the host store provably unreachable from the guest (SEC-03).
- **`mgit work`** — one command to start an agent on a task: provisions a task-bound worktree and wires the agent's shell to route through `mgit run` into the sandbox (degrades gracefully when no backend is present).
- **`mgit run` / `mgit sandbox …`** — route execution into the task sandbox; launch/list/exec/land/grant lifecycle.
- **Runs over an existing git repo (MGIT-14)** — self-contained `.mgit` store via go-git plumbing; the project's `.git` is provably never mutated.
- **Git-authoritative auto-housekeeping (ADR-008, MGIT-35)** — mgit keeps its `.mgit` base coherent with your current local working state automatically; **no manual `mgit sync`**. New worktrees carry your unpushed local foundation; each task pins its fork-base so a later resync never corrupts its diff; defensive read-only `.git` access.
- **Worktree materialization** — a worktree is a full working copy seeded from `.mgit` (gitignore-aware); `.mgit/seed-include` carries build-required gitignored artifacts (e.g. an embedded `web/dist`). (MGIT-38)
- **REST + MCP sandbox surface**, agent-integration adapters, and `mgit docs generate` agent artifacts.

### Changed / Fixed

- **`mgit version`** now reports real build metadata (version/commit/date via ldflags, with a `debug.ReadBuildInfo` fallback for `go install`/`go build`) instead of `dev (commit: none, built: unknown)`. (MGIT-40)
- **Task-id flag standardized** — `--task-id` is canonical on every command; `--task` is accepted as a hidden back-compat alias. (MGIT-37)
- **`mgit squash --to-git` round-trips** — emits a real `git diff` patch that `git apply` / `git am` accepts byte-for-byte. (MGIT-33)
- **`mgit add` / `mgit status` honor `.gitignore`** — no more staging build junk. (MGIT-32)
- **Task-id grammar broadened** — accepts ids like `MTIX-30-probe` and `MTIX-30.6`, with a clear, actionable error on unsafe input. (MGIT-41)
- Positioning corrected away from "safety-critical / DO-178C": mgit is the checkpointed, sandboxed working substrate for LLM coding agents.

### Known limitations

- The microVM sandbox is **Linux + macOS** in this beta; Windows runs core mgit (worktrees / commit / squash) without the sandbox (WCOW backend planned).
- macOS containment runs a **Linux guest** via Virtualization.framework; a mac-native profile is planned.
- The backtrack / fork / cherry-pick course-correction loop is cheap and instructed, but not yet validated as something agents reach for autonomously — reviewer-driven today.

## [0.1.0-beta]

### Added

- **CLI**: 22 commands covering the full mgit workflow — init, commit, log, status, show, branch, config, squash, rollback, verify, audit, add, export, cherry-pick, restore, checkout, merge, gc, import, worktree, docs generate
- **REST API**: 10 endpoints on localhost:6860 with Bearer token authentication, ULID request IDs, and JSON error responses
- **MCP Server**: 15 tools for LLM agent integration via stdio transport (commit, rollback, squash, status, log, show, branch, verify, diff, export, audit, config, worktree add/list/remove)
- **mtix Integration**: HTTP client for mtix REST API, bidirectional task-commit synchronization, auto-squash on task completion
- **Agent Worktrees**: Linked worktree support for multi-agent parallel development with task binding isolation (FR-16)
- **Documentation Generator**: `mgit docs generate` produces 9 agent-facing documentation files (CLI reference, MCP tools, SKILL.md, workflow guides, troubleshooting)
- **Token Authentication**: `mgit token generate/rotate/revoke/list` with SHA-256 hash storage and Bearer middleware
- **Integrity Verification**: Dual-hash model (SHA-1 + SHA-256), commit chain verification, index consistency checks
- **Append-Only Audit**: Immutable task_commits table, structured audit log, rollbacks via revert commits
- **Build Pipeline**: GoReleaser cross-compilation (6 platforms), GitHub Actions CI/CD, cosign signing, Homebrew tap integration

### Performance

- Commit creation: 0.39ms (target <5ms)
- Log query (100 commits): 1.1ms (target <50ms)
- Squash (10 commits): 0.63ms (target <500ms)
- Verify (50 commits): 0.61ms (target <1s)

### Technical

- Pure Go, zero CGO dependencies
- go-git v5 for embedded git engine (no external git binary)
- modernc.org/sqlite for pure Go SQLite (WAL mode, SYNCHRONOUS=FULL)
- 530+ tests, zero race conditions, zero lint warnings
- 85%+ code coverage across all packages
