# Changelog

All notable changes to mgit are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
