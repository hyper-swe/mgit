<p align="center">
  <h1 align="center">mgit</h1>
  <p align="center">
    <strong>Let coding agents run untrusted code &mdash; in a disposable microVM, not on your machine</strong>
  </p>
  <p align="center">
    Autonomous agents <code>npm install</code>, build, and run code on every task. mgit runs that execution inside a per-task microVM, so a compromised dependency burns a throwaway VM &mdash; not your SSH keys, cloud credentials, or the rest of your disk. Only verified, task-tagged changes land back in your real repo.
  </p>
  <p align="center">
    <a href="#the-problem-agents-run-untrusted-code-on-your-machine">The Problem</a> &middot;
    <a href="#how-mgit-contains-it">Containment</a> &middot;
    <a href="#installation">Install</a> &middot;
    <a href="#quick-start">Quick Start</a> &middot;
    <a href="#commands">Commands</a> &middot;
    <a href="#security-model">Security</a> &middot;
    <a href="#architecture">Architecture</a>
  </p>
</p>

---

## The Problem: agents run untrusted code on your machine

Every time you let a coding agent work a task in auto mode, it runs code you never read:

> *"add the auth library and wire it up"*
>
> &mdash; *the agent runs `npm install`, which executes the postinstall scripts of 200 transitive dependencies; then it runs the build and the test suite*

All of that executes **on your host**, with your user's privileges &mdash; the same machine that holds your `~/.ssh` keys, your `~/.aws` credentials, your browser sessions, and every other repository you own. The npm and PyPI ecosystems ship malware on a weekly cadence (typosquats, hijacked maintainers, malicious postinstall hooks). A single compromised dependency, the *first* time an agent installs it, can exfiltrate those secrets or plant persistence. That loss is **irrecoverable** &mdash; you cannot roll back a stolen key.

The agent doesn't have to be malicious. The code it runs on your behalf just has to contain one bad package.

## How mgit contains it

mgit runs the agent's untrusted execution inside a **per-task microVM** &mdash; Firecracker on Linux/KVM, Apple Virtualization.framework on macOS &mdash; so the blast radius of a compromised package is a disposable VM, not your host:

- **Hardware-isolated execution.** Installs, builds, and tests run in the guest VM. The host filesystem, your other repos, and your credentials are never mounted in. The microVM boundary is the same one cloud providers trust to isolate tenants.
- **Default-deny egress.** The guest gets no direct network route. A per-task allowlist permits only the destinations a task actually needs (e.g. your package registry), enforced at the IP/flow layer by a host-side proxy &mdash; raw-IP, QUIC, DNS-tunnelling, and metadata-endpoint tricks are denied. (`none` / `allowlist` / `open` modes.)
- **A verified airlock back to your repo.** The agent commits inside the sandbox; only its changes are pulled back over a dedicated channel, re-verified host-side (dual-hash + task binding + a host-anchored attestation the guest cannot forge), and appended to your real repository. Nothing the guest produces reaches your repo unverified.
- **Seamless, fail-closed routing.** `mgit run -- <agent command>` transparently routes the agent's execution into the task's sandbox; if the sandbox is unavailable it fails closed (it never silently runs on the host). Adapters wire this into Claude Code, Codex, and Cursor without harness changes.

This is mgit's first job: **make running agents in auto mode safe by default.** The version-control layer below is the airlock that lets contained work flow back out cleanly.

> **Security posture.** The hardware-isolation boundary is the load-bearing guarantee and has been adversarially audited (see [`AUDIT-FR17-SANDBOX-SECURITY-V1.md`](AUDIT-FR17-SANDBOX-SECURITY-V1.md)). The seam-level defenses (quarantine of the host object store, egress enforcement, the land path) are under continuous, independently-reviewed hardening &mdash; mgit treats "never trust the guest side of a seam" as a standing law, not a checkbox. The sandbox ships for Linux and macOS; on Windows, mgit's core version control runs without the sandbox until the native backend lands.

## The second pillar: surgical rollback

Contained execution gets work *in* safely. The other half is undoing a wrong decision without throwing away the good work around it. Because every step is preserved as an addressable, task-tagged commit, you rewind to the exact decision point and branch from there &mdash; instead of reprompting the agent to rewrite hundreds of lines from scratch:

```
prompt -> commit -> commit -> commit -> wrong choice -> commit -> commit
                              ^                                  ^
                              |                                  |
                              +-- rollback to here ---------------+
                                     |
                                     +-- branch and continue from this point
                                         (zero wasted work below the rollback point)
```

The agent's correct decisions are preserved; only the wrong branch is undone. **The cost of an agent's mistake is bounded by the cost of the wrong decision, not the cost of the entire task** &mdash; and the rolled-back attempt stays in the append-only audit trail.

## Why mgit?

mgit is purpose-built for autonomous coding agents working on real codebases:

- **Sandboxed execution** &mdash; Untrusted installs/builds/tests run in a per-task microVM, not on your host. A compromised package is contained to a disposable VM.
- **Default-deny egress** &mdash; Per-task network allowlist enforced at the IP/flow layer; the guest cannot reach your LAN, cloud metadata, or arbitrary hosts unless explicitly permitted.
- **Verified land** &mdash; Only changes that pass host-side re-verification (dual-hash + task binding + host-anchored attestation) land in your repo.
- **Surgical rollback** &mdash; Roll back a single task while preserving every other commit on the branch; continue from the rollback point with full context intact.
- **Task traceability** &mdash; Every commit is bound to a task ID. Query any task to see exactly which commits it produced, in which sandbox image, under which network posture.
- **Append-only audit trail** &mdash; Commits are never deleted; rollbacks create revert commits. The history of *every* attempt is preserved for review.
- **Multi-agent isolation** &mdash; Linked worktrees + per-task sandboxes let multiple agents work different tasks in parallel without stepping on each other.
- **Runs over your existing git repo** &mdash; mgit keeps a self-contained `.mgit/` store and provably never touches your project's `.git`.
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

### Run an agent in a sandbox

```bash
# Create an isolated worktree bound to a task
mgit worktree add ./wt-PROJ-1 --task=PROJ-1

# Run the agent's command inside the task's microVM (fail-closed: never
# falls back to the host). Installs/builds/tests execute in the guest.
mgit run --task=PROJ-1 -- npm install && npm test

# Pull the verified changes back into your repo (the airlock)
mgit sandbox land --task=PROJ-1
```

Agent harnesses (Claude Code, Codex, Cursor) can be wired to route commands through `mgit run` automatically at worktree creation, so sandboxing is transparent to the agent.

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

### Sandbox / Agent Execution

| Command | Description |
|---------|-------------|
| `mgit run --task=ID -- <command>` | Run a command inside the task's microVM (fail-closed; never runs on the host) |
| `mgit sandbox launch --task=ID --worktree=PATH --image=REF` | Provision a sandbox for a task |
| `mgit sandbox exec --task=ID -- <command>` | Execute one command in the task's sandbox |
| `mgit sandbox shell --task=ID` | Attach an interactive session (T2 confined-agent mode) |
| `mgit sandbox land --task=ID` | Pull + host-verify + land the sandbox's changes into your repo |
| `mgit sandbox status TASK-ID` / `list` / `remove TASK-ID` | Inspect or tear down sandboxes |
| `mgit sandbox grants --task=ID` / `grant --task=ID KEY` | Review and approve per-task egress capability requests |
| `mgit sandbox image init` / `add --kernel … --rootfs …` | Manage the signed, digest-pinned guest image set |

> Sandbox commands require the host daemon and a guest image, and run on Linux (Firecracker/KVM) and macOS (Virtualization.framework). See [Security Model](#security-model).

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

## Security Model

mgit's sandbox is designed around one premise: **the guest is the hostile party** &mdash; it runs the untrusted dependency code, so nothing it produces or asserts is trusted. The host is the trust anchor. Every guarantee is enforced host-side, at the four places host and guest meet:

| Seam | Control |
|------|---------|
| **Execution boundary** | Per-task microVM (Firecracker / Virtualization.framework). Untrusted installs/builds/tests run in the guest; the host disk and credentials are never mounted in. |
| **Network egress** | Default-deny at the IP/flow layer. Per-task allowlist via a host proxy + restricted DNS; RFC1918, link-local, and cloud-metadata destinations denied unconditionally; UDP/QUIC blocked. |
| **Worktree mount** | The guest sees working-tree files only; the host's shared object store, index, and other tasks' data are not part of the guest view. |
| **Land / attestation** | Commits are re-verified host-side (dual-hash + task binding) and carry a **host-anchored attestation** &mdash; the guest holds no signing key and cannot forge provenance. Land is the only path from the guest's private store to your repo, and it is append-only. |

Additional properties: guest images are digest-pinned **and** Ed25519-signature-verified at boot; capability escalations (extra egress) are derived only from the host-observed denied connection, scoped to the sandbox lifetime, and audited &mdash; there is no "allow all"; the local daemon socket is same-UID peer-credential authenticated; a global concurrency + memory ceiling bounds host resource use.

This model has been **adversarially audited** (red-team design audit + an independent story-closure code review against each control). The audit anchors are checked into the repo ([`AUDIT-FR17-SANDBOX-V1.md`](AUDIT-FR17-SANDBOX-V1.md), [`AUDIT-FR17-SANDBOX-SECURITY-V1.md`](AUDIT-FR17-SANDBOX-SECURITY-V1.md)). The hardware-isolation boundary that protects your host is sound; the seam-level defenses are under continuous, independently-reviewed hardening. We publish the controls we enforce and treat open findings as release-gating.

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
- **Pure-Go core** &mdash; No CGO, no external git binary in the core. Single static binary, cross-compiles to 6 platforms. The sandbox runs as a separate privileged host daemon (`mgit-sandboxd`) driving the microVM backends; any platform CGO (macOS Virtualization.framework) is confined there, so the core stays pure Go.

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
