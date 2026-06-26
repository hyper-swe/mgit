# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
