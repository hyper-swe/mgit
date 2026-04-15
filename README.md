<p align="center">
  <h1 align="center">mgit</h1>
  <p align="center">
    <strong>Surgical rollback for LLM coding agents</strong>
  </p>
  <p align="center">
    Stop wasting tokens rewriting working code. Roll back the wrong decision, keep everything else.
  </p>
  <p align="center">
    <a href="#the-problem-mgit-solves">The Problem</a> &middot;
    <a href="#installation">Install</a> &middot;
    <a href="#quick-start">Quick Start</a> &middot;
    <a href="#commands">Commands</a> &middot;
    <a href="#mcp-integration">MCP</a> &middot;
    <a href="#architecture">Architecture</a>
  </p>
</p>

---

## The Problem mgit Solves

When an LLM coding agent works on a task without version control, a single bad decision &mdash; the wrong library, the wrong data structure, the wrong abstraction &mdash; cascades into wasted work:

> *"Use the React Context API for this state management."*
>
> &mdash; *the agent writes 800 lines of code, 12 components, 6 tests*
>
> *"Actually, use Zustand instead."*
>
> &mdash; *the agent rewrites all 800 lines from scratch, burning tokens, time, and your patience*

The problem isn't the agent. It's that a fresh prompt is the only tool the agent has to "fix" its previous work. There's no way to say *"keep the components, keep the tests, just swap out the state management layer."*

**mgit fixes this with surgical rollback.** When you instruct the agent to make micro-commits via mgit during the task, every step of its work is preserved as an addressable, task-tagged commit. When something goes wrong, you don't restart &mdash; you rewind to the exact decision point, branch from there, and continue.

### Without mgit

```
prompt -> 800 lines of code -> wrong choice -> reprompt -> 800 lines rewritten
                                                            (wasted time + tokens)
```

### With mgit

```
prompt -> commit -> commit -> commit -> wrong choice -> commit -> commit
                              ^                                  ^
                              |                                  |
                              +-- rollback to here ---------------+
                                     |
                                     +-- branch and continue from this point
                                         (zero wasted work below the rollback point)
```

The agent's correct decisions are preserved. Only the wrong branch is undone. Work resumes from a known-good state with full context intact.

This is what makes AI-driven development viable for enterprise codebases: **the cost of an agent's mistake is bounded by the cost of the wrong decision, not the cost of the entire task.**

## Why mgit?

mgit is purpose-built for LLM coding agents working on real codebases:

- **Surgical rollback** &mdash; Roll back a single task while preserving every other commit on the branch. Continue from the rollback point as a new branch with full context intact.
- **Task traceability** &mdash; Every commit is bound to a task ID. Query any task to see exactly which commits it produced.
- **Append-only audit trail** &mdash; Commits are never deleted. Rollbacks create revert commits. The history of *every* attempt is preserved for review.
- **Squash workflow** &mdash; Consolidate dozens of micro-commits into a single reviewable commit when a task is verified correct.
- **Multi-agent isolation** &mdash; Linked worktrees let multiple agents work on different tasks in parallel without stepping on each other.
- **Integrity verification** &mdash; Dual-hash model (SHA-1 for git compatibility, SHA-256 for tamper detection) with chain verification.
- **Three integration modes** &mdash; CLI for humans, REST API for services, MCP tools for AI agents.

## Installation

### Homebrew (macOS / Linux)

```bash
brew install hyper-swe/tap/mgit
```

### Go Install

```bash
go install github.com/hyper-swe/mgit/cmd/mgit@latest
```

### From Source

```bash
git clone https://github.com/hyper-swe/mgit.git
cd mgit
make build
./cmd/mgit/mgit --version
```

### Binary Releases

Download pre-built binaries from [GitHub Releases](https://github.com/hyper-swe/mgit/releases). Available for Linux, macOS, and Windows on amd64 and arm64.

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
| `mgit squash --task-id=ID [--to-git \| --to-main]` | Consolidate micro-commits into one |
| `mgit rollback --task-id=ID [--commit=HASH]` | Revert task or specific commit (append-only) |
| `mgit verify [--task-id=ID] [--fix]` | Verify commit chain and index integrity |
| `mgit audit [--task-id=ID] [--since --until]` | View the audit trail |
| `mgit export --task-id=ID --format=json\|git\|audit-log` | Export task data in multiple formats |

### Multi-Agent

| Command | Description |
|---------|-------------|
| `mgit worktree add PATH --task=ID [--branch]` | Create isolated worktree for an agent |
| `mgit worktree list [--porcelain]` | List active worktrees |
| `mgit worktree remove PATH [--force]` | Remove a worktree |
| `mgit worktree prune [--dry-run]` | Remove stale worktree metadata |

### Additional

| Command | Description |
|---------|-------------|
| `mgit add [files...] [--all]` | Stage files |
| `mgit diff [--from --to \| --task-id \| --staged]` | Show differences between commits, tasks, or staged files |
| `mgit checkout BRANCH` | Switch branches (blocks on uncommitted changes) |
| `mgit merge BRANCH [--squash \| --no-ff]` | Merge with fast-forward, squash, or no-ff strategy |
| `mgit cherry-pick HASH [--no-commit \| --onto]` | Apply a commit to current or target branch |
| `mgit restore FILE --commit=HASH` | Restore a single file from a commit |
| `mgit gc [--aggressive]` | Pack loose objects and report space saved |
| `mgit import --file=BUNDLE [--mode=merge\|replace]` | Import a bundle with SHA-256 manifest verification |
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

## mgit + mtix: Enterprise-Grade AI Coding

mgit pairs with [mtix](https://github.com/hyper-swe/mtix), an AI-native micro issue manager, to form a closed loop that makes LLM-driven development viable for enterprise codebases.

The two tools answer the questions that matter most:

- **mtix** &mdash; *what was supposed to happen?* (the task, the acceptance criteria, who claimed it)
- **mgit** &mdash; *what actually happened?* (the commits, the diffs, the agent, the timestamps)

Task IDs flow seamlessly between both systems. The combined workflow looks like this:

1. **mtix** decomposes a feature into micro-tasks with clear acceptance criteria
2. **An LLM agent** claims a task in mtix
3. **mgit** records every step of the agent's work as task-tagged micro-commits
4. **When something goes wrong** &mdash; wrong library, wrong abstraction, regression introduced &mdash; you rollback that single task in mgit, branch from the rollback point, and reprompt the agent with refined instructions
5. **Other tasks on the same branch keep their work intact**, even if their commits came after the rolled-back task
6. **When mtix marks the task done**, mgit auto-squashes the (now-correct) work into a single reviewable commit
7. **The full history** &mdash; including the rolled-back attempts &mdash; remains in the audit trail for review

This is how AI-driven development scales beyond toy projects: **the unit of failure is a task, not a session.** Bad decisions are bounded, recoverable, and surgically replaceable.

```bash
# 1. Find work in mtix
mtix ready

# 2. Claim a task
mtix claim PROJ-4.2.1 --agent=claude-01

# 3. Make tagged commits in mgit
mgit commit --task-id=PROJ-4.2.1 --agent-id=claude-01 --message="add validation"
mgit commit --task-id=PROJ-4.2.1 --agent-id=claude-01 --message="add tests"

# 4. Mark done in mtix — triggers mgit auto-squash
mtix done PROJ-4.2.1
```

**The combination delivers:**

| Capability | mtix alone | mgit alone | mgit + mtix |
|------------|-----------|------------|-------------|
| Track what to build | Yes | No | Yes |
| Track what was built | No | Yes | Yes |
| Link requirements to commits | No | No | Yes |
| Multi-agent task assignment | Yes | No | Yes |
| Multi-agent code isolation | No | Yes | Yes |
| Audit trail of decisions | Yes | No | Yes |
| Audit trail of code changes | No | Yes | Yes |
| Auto-squash on completion | No | Yes | Yes (event-driven) |
| Regulatory traceability | Partial | Partial | Complete |

For LLM-driven development in regulated environments, this combination is the difference between *"the AI did some work"* and *"agent claude-01 implemented requirement PROJ-4.2.1 across these 7 commits, squashed at this timestamp, verified by chain hash X."*

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
                 | (13 services)   |
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

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.

Copyright 2025-2026 [HyperSWE](https://github.com/hyper-swe)
