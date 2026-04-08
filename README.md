<p align="center">
  <h1 align="center">mgit</h1>
  <p align="center">
    <strong>Safety-critical micro version control for LLM coding agents</strong>
  </p>
  <p align="center">
    <a href="#installation">Install</a> &middot;
    <a href="#quick-start">Quick Start</a> &middot;
    <a href="#commands">Commands</a> &middot;
    <a href="#mcp-integration">MCP</a> &middot;
    <a href="#architecture">Architecture</a>
  </p>
</p>

---

mgit (micro git) is a task-tagged version control system purpose-built for LLM coding agents. Every commit is bound to a task ID, creating an immutable audit trail that traces exactly which code changes were made for which task, by which agent, and when.

Built for environments where provenance matters: regulated industries, safety-critical systems, and any team that needs to answer *"what changed and why?"* with certainty.

## Why mgit?

When LLM agents write code, you need more than git:

- **Task traceability** &mdash; Every commit is tagged with a task ID. Query any task to see exactly which commits it produced.
- **Append-only audit trail** &mdash; Commits are never deleted. Rollbacks create revert commits. History is immutable.
- **Squash workflow** &mdash; Consolidate dozens of micro-commits into a single reviewable commit when a task is done.
- **Multi-agent isolation** &mdash; Linked worktrees let multiple agents work on different tasks in parallel without interference.
- **Integrity verification** &mdash; Dual-hash model (SHA-1 for git compatibility, SHA-256 for tamper detection) with chain verification.
- **Three integration modes** &mdash; CLI for humans, REST API for services, MCP tools for AI agents.

## Installation

### Homebrew (macOS / Linux)

```bash
brew install hyper-swe/tap/mgit
```

### Go Install

```bash
go install github.com/hyper-swe/mgit-dev/cmd/mgit@latest
```

### From Source

```bash
git clone https://github.com/hyper-swe/mgit-dev.git
cd mgit-dev
make build
./cmd/mgit/mgit --version
```

### Binary Releases

Download pre-built binaries from [GitHub Releases](https://github.com/hyper-swe/mgit-dev/releases). Available for Linux, macOS, and Windows on amd64 and arm64.

## Quick Start

```bash
# Initialize a repository
mgit init

# Make task-tagged commits
mgit commit --task-id=PROJ-1.2.3 --message="add user validation"
mgit commit --task-id=PROJ-1.2.3 --message="add unit tests"
mgit commit --task-id=PROJ-1.2.3 --message="fix edge case"

# View task history
mgit log --task-id=PROJ-1.2.3

# Squash when the task is done
mgit squash --task-id=PROJ-1.2.3

# Verify integrity
mgit verify
```

## Commands

### Core

| Command | Description |
|---------|-------------|
| `mgit init` | Initialize a new mgit repository |
| `mgit commit --task-id=ID` | Create a task-tagged micro-commit |
| `mgit log [--task-id=ID]` | View commit history, optionally filtered by task |
| `mgit status` | Show working tree status |
| `mgit show HASH` | Display commit details |
| `mgit branch --task-id=ID` | Create a task branch |
| `mgit branch` | List all branches |
| `mgit config get/set/list` | Manage configuration |

### Workflows

| Command | Description |
|---------|-------------|
| `mgit squash --task-id=ID` | Consolidate micro-commits into one |
| `mgit rollback --task-id=ID` | Revert task changes (append-only) |
| `mgit verify [--task-id=ID]` | Verify commit chain and index integrity |
| `mgit audit [--task-id=ID]` | View the audit trail |
| `mgit export --task-id=ID` | Export task commits as JSON |

### Multi-Agent

| Command | Description |
|---------|-------------|
| `mgit worktree add PATH --task=ID` | Create isolated worktree for an agent |
| `mgit worktree list` | List active worktrees |
| `mgit worktree remove PATH` | Remove a worktree |

### Additional

| Command | Description |
|---------|-------------|
| `mgit add [files...] [--all]` | Stage files |
| `mgit checkout BRANCH` | Switch branches |
| `mgit merge BRANCH` | Merge a branch |
| `mgit cherry-pick HASH` | Apply a commit to current branch |
| `mgit restore FILE --commit=HASH` | Restore a file from a commit |
| `mgit docs generate` | Generate agent-facing documentation |

All commands support `--json` for structured output.

## MCP Integration

mgit exposes 15 MCP tools for direct use by LLM coding agents:

```
mgit_commit      mgit_rollback     mgit_squash       mgit_status
mgit_log         mgit_show         mgit_branch       mgit_verify
mgit_diff        mgit_export       mgit_audit        mgit_config
mgit_worktree_add   mgit_worktree_list   mgit_worktree_remove
```

### Configure as MCP Server

**Claude Code:**
```bash
claude mcp add mgit -- mgit mcp --project /path/to/your/project
```

**Cursor** (`.cursor/mcp.json`):
```json
{
  "mcpServers": {
    "mgit": {
      "command": "mgit",
      "args": ["mcp", "--project", "/path/to/your/project"]
    }
  }
}
```

## REST API

Start the API server:

```bash
mgit serve --port=6860
```

Endpoints:

```
GET  /health                  Health check
POST /api/v1/commits          Create commit
GET  /api/v1/commits/:id      Get commit
GET  /api/v1/commits          List commits
GET  /api/v1/tasks/:id/commits Task commits
GET  /api/v1/branches         List branches
POST /api/v1/branches         Create branch
POST /api/v1/squash           Squash task
POST /api/v1/rollback         Rollback task
GET  /api/v1/verify           Verify integrity
```

All endpoints return JSON. Authentication via Bearer token (`mgit token generate`).

## mtix Integration

mgit integrates with [mtix](https://github.com/hyper-swe/mtix) for task management. Task IDs are shared between both systems:

```bash
# In mtix: find a task
mtix ready

# In mgit: commit against it
mgit commit --task-id=PROJ-4.2.1 --message="implement feature"

# When task is done in mtix, mgit can auto-squash
```

## Architecture

```
                    +-----------+
                    |  CLI (22) |
                    +-----+-----+
                          |
            +-------------+-------------+
            |                           |
      +-----+-----+           +--------+--------+
      | REST API   |           | MCP Server (15) |
      | (10 routes)|           | (stdio/SSE)     |
      +-----+------+           +--------+--------+
            |                           |
            +-------------+-------------+
                          |
                 +--------+--------+
                 |  Service Layer  |
                 |  (8 services)   |
                 +--------+--------+
                          |
              +-----------+-----------+
              |                       |
       +------+------+        +------+------+
       |   go-git    |        |   SQLite    |
       |   Store     |        |   Index     |
       +------+------+        +------+------+
              |                       |
         .mgit/objects           .mgit/index.db
         .mgit/refs
```

**Design principles:**

- **Layered architecture** &mdash; CLI/API/MCP call services; services call stores; stores manage go-git and SQLite. No layer skipping.
- **Append-only** &mdash; The `task_commits` table and audit log are insert-only. Rollbacks create new commits, never delete.
- **Dual-hash integrity** &mdash; SHA-1 for git protocol compatibility, SHA-256 for content verification and tamper detection.
- **Clock injection** &mdash; All timestamps come from injected clocks, enabling deterministic testing.
- **Pure Go** &mdash; No CGO, no external git binary. Single static binary. Cross-compiles to 6 platforms.

## Configuration

Stored in `.mgit/config.json`. Manage via CLI:

```bash
mgit config list                    # Show all settings
mgit config get api.http_port       # Get a value
mgit config set logging.level debug # Set a value
```

Key settings:

| Key | Default | Description |
|-----|---------|-------------|
| `project.prefix` | `MGIT` | Task ID prefix |
| `api.http_port` | `6860` | REST API port |
| `api.bind_address` | `127.0.0.1` | API bind address (localhost only by default) |
| `squash.auto_notify` | `true` | Notify mtix on squash |
| `rollback.auto_reopen` | `true` | Reopen tasks on rollback |
| `branch.auto_create` | `true` | Auto-create branch on first task commit |

## Development

```bash
make test          # Run tests
make test-race     # Run with race detector
make test-cover    # Coverage report
make lint          # Run linter
make bench         # Run benchmarks
make preflight     # Full pre-release quality checks
make build         # Build binary
```

### Performance

Benchmarked on Apple M5:

| Operation | Time | Target |
|-----------|------|--------|
| Commit | 0.39ms | <5ms |
| Log (100 commits) | 1.1ms | <50ms |
| Squash (10 commits) | 0.63ms | <500ms |
| Verify (50 commits) | 0.61ms | <1s |

## Safety-Critical Design

mgit is built for environments where audit compliance and code provenance are non-negotiable. Its design aligns with the principles of:

| Standard | Domain | What mgit provides |
|----------|--------|-------------------|
| **DO-178C** | Avionics | Immutable audit trail linking every code change to a task |
| **IEC 62304** | Medical devices | Append-only history that cannot be rewritten or deleted |
| **NASA-STD-8739.8** | Spaceflight | Integrity verification with dual-hash chain validation |
| **MIL-STD-498** | Defense acquisition | Full traceability from requirement to commit to test |

mgit is a **development tool**, not embedded software. It does not require certification itself, but it produces the artifacts (audit logs, traceability records, integrity proofs) that certified systems need. When a regulator asks *"what code changed for this patient safety task?"*, mgit provides the answer with cryptographic certainty.

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.

Copyright 2025-2026 [HyperSWE](https://github.com/hyper-swe)
