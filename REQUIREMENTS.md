# mgit — Requirements Specification

**Version:** 1.0
**Author:** Vimal Menon
**Date:** March 9, 2026
**Status:** Draft
**Classification:** Safety-Critical Software Specification
**Companion Product:** mtix (micro-tix) — AI-native micro issue manager

---

## 1. Vision

**mgit** (micro git) is a safety-critical micro version control system designed for LLM coding agents operating within the mtix ecosystem. Where mtix decomposes work into granular micro-issues with dot-notation IDs (e.g., `PROJ-4.2.1.3.4`), mgit provides **task-tagged micro-commits** that map 1:1 to those micro-issues — enabling per-task rollback, granular audit trails, and clean squash-to-git workflows.

### 1.1 The Problem

When LLM coding agents work on micro-issues in mtix, they commit code to standard git repositories. This creates several problems in safety-critical environments:

1. **Granularity mismatch:** A single git commit may span multiple micro-issues, or a micro-issue may require multiple git commits. There is no clean mapping between task completion and version control.

2. **Risky rollback:** If a review catches a mistake at micro-issue `PROJ-4.2.1.3`, reverting in git may undo unrelated work from other agents or tasks. Selective rollback per micro-issue is impossible.

3. **Production repository pollution:** Development teams in NASA, airline, hospital, and DoD environments may not permit LLM agents to commit directly to production git servers. Creating sandboxes and worktrees for each agent is operationally expensive.

4. **Lost audit trail:** Standard git doesn't capture which mtix task ID each commit belongs to, what agent made it, or what session context existed at commit time.

### 1.2 The Solution

mgit provides an **embedded git server** (via go-git, pure Go) that lives alongside the mtix project directory. Every micro-commit is tagged with the mtix task ID, agent ID, and session context. When a task is complete and reviewed, all micro-commits for that task are **squashed into a single standard git commit** that can be applied to the production repository.

### 1.3 Core Value Propositions

- **Per-task rollback:** Revert to the exact state after any micro-task completion, without affecting other tasks
- **No production repo pollution:** All micro-commits live in `.mgit/`, isolated from the production git repository
- **Clean squash-to-git:** Upon task completion, collapse N micro-commits into 1 standard git commit with full traceability
- **Append-only audit trail:** Every micro-commit is permanent — rollbacks create new revert commits, never delete history
- **Seamless mtix integration:** Shared task IDs, MCP tool interop, event-driven workflows

### 1.4 Applicable Standards

mgit is developed to the same safety-critical standards as mtix:

- **DO-178C** — Software Considerations in Airborne Systems and Equipment Certification
- **IEC 62304** — Medical Device Software — Software Life Cycle Processes
- **NASA-STD-8739.8** — Software Assurance and Software Safety Standard
- **MIL-STD-498** — Software Development and Documentation
- **OWASP ASVS Level 2** — Application Security Verification Standard

### 1.5 Relationship to mtix

```
┌─────────────────────────────────────────────────┐
│                 Project Directory                │
│                                                  │
│  .mtix/          ← mtix project data             │
│  │  config.yaml                                  │
│  │  data/mtix.db                                 │
│  │  logs/                                        │
│                                                  │
│  .mgit/          ← mgit repository data          │
│  │  config.yaml                                  │
│  │  objects/     ← go-git object store           │
│  │  refs/        ← branch references             │
│  │  index.db     ← SQLite task-commit mapping    │
│  │  HEAD                                         │
│  │  audit.log    ← append-only audit trail       │
│                                                  │
│  src/            ← actual project source code     │
│  docs/                                           │
│  .git/           ← production git repository     │
└─────────────────────────────────────────────────┘
```

mgit and mtix are **independent binaries** that communicate via MCP tools and REST API. They share task ID conventions but maintain separate data stores.

---

## 2. Functional Requirements

### FR-1: Repository Model

**FR-1.1** mgit MUST store all data in a `.mgit/` directory at the project root, co-located with `.mtix/` and `.git/`.

**FR-1.2** The `.mgit/` directory structure MUST be:
```
.mgit/
├── config.yaml       # mgit configuration (non-sensitive settings only)
├── tokens.json       # API authentication tokens (0600 permissions, hashed)
├── objects/          # go-git object store (blobs, trees, commits, tags)
│   ├── pack/         # packfiles for compression
│   └── info/         # object info
├── refs/             # branch and tag references
│   ├── heads/        # branch tips
│   └── tags/         # tag references
├── HEAD              # current branch reference
├── index.db          # SQLite database for task-commit mapping
├── audit.log         # append-only audit trail
└── locks/            # PID lock files for concurrent access
```

**FR-1.3** `mgit init` MUST:
1. Create the `.mgit/` directory structure
2. Initialize a bare go-git repository in `.mgit/`
3. Create the SQLite index database with schema
4. Create `config.yaml` with default values
5. Auto-detect `.mtix/` in the same directory and configure integration if found
6. Create an initial empty commit on the `main` branch
7. Write the project prefix to config if `.mtix/` is detected

**FR-1.3a** If `.mgit/` already exists, `mgit init` MUST return an error: `"mgit repository already initialized in {path}"`

**FR-1.4** mgit MUST support a PID lock file (`.mgit/locks/mgit.lock`) to prevent concurrent write access. The lock file contains the PID of the owning process and a timestamp. Stale locks (process not running) MUST be automatically cleaned up.

**FR-1.5** mgit MUST auto-detect the `.mgit/` directory by walking up the directory tree from the current working directory, identical to how git finds `.git/`.

---

### FR-2: Micro-Commit System

**FR-2.1** Every mgit commit MUST be tagged with an mtix task ID in dot-notation format. The task ID follows the pattern: `^[A-Z][A-Z0-9-]{0,19}-\d+(\.\d+)*$` (e.g., `PROJ-4.2.1.3`).

**FR-2.1a** The task ID is stored in the commit message prefix AND in structured commit metadata (go-git commit extra headers or notes).

**FR-2.2** Commit messages MUST follow the format:
```
[MGIT:{TASK_ID}] {user_message}

Agent: {agent_id}
Session: {session_id}
Timestamp: {ISO8601}
Parent-Task: {parent_task_id}
Files-Changed: {count}
```

**FR-2.3** mgit MUST auto-generate commit messages when none is provided. The auto-generated format is:
```
[MGIT:{TASK_ID}] Auto-commit for {task_title}
```
where `task_title` is retrieved from mtix via MCP tool `mtix_show` if available, otherwise the task ID is used as title.

**FR-2.4** Each commit MUST be content-addressed using SHA-256 of the tree hash + parent hash + metadata. This is the commit's unique identifier.

**FR-2.5** mgit MUST track the following file operations in each commit:
- Added files (new files)
- Modified files (changed content)
- Deleted files (removed files)
- Renamed files (detected via content similarity threshold ≥90%)

**FR-2.6** mgit MUST support staging files before commit:
- `mgit add <file>` — stage a specific file
- `mgit add .` — stage all changed files
- `mgit add --task <TASK_ID>` — stage all files associated with the task's working directory scope

**FR-2.6a** If no files are staged and `mgit commit` is called, mgit MUST return an error: `"nothing to commit — stage files with 'mgit add' first"`.

**FR-2.7** mgit MUST reject commits with an empty changeset (no file modifications). This prevents no-op commits from polluting the audit trail.

**FR-2.8** mgit MUST support `--allow-empty` flag to override FR-2.7 for system-generated commits (e.g., rollback markers, squash markers).

---

### FR-3: Commit Data Model

**FR-3.1** Each mgit commit MUST contain the following fields:

| Category | Field | Type | Description |
|----------|-------|------|-------------|
| **Identity** | commit_id | TEXT (SHA-256) | Content-addressed unique identifier |
| **Identity** | short_id | TEXT (first 8 chars) | Human-readable short form |
| **Lineage** | parent_id | TEXT (nullable) | Parent commit SHA-256 (null for initial) |
| **Lineage** | tree_hash | TEXT (SHA-256) | Hash of the file tree at this commit |
| **Task** | task_id | TEXT | mtix dot-notation task ID (required) |
| **Task** | parent_task_id | TEXT (nullable) | Parent task ID (e.g., PROJ-4.2.1 for PROJ-4.2.1.3) |
| **Agent** | agent_id | TEXT | ID of the agent/user who created the commit |
| **Agent** | session_id | TEXT (nullable) | mtix session ID if available |
| **Content** | message | TEXT | Commit message (with [MGIT:TASK_ID] prefix) |
| **Content** | file_diffs | JSON | Array of {path, operation, old_hash, new_hash} |
| **Content** | files_changed | INTEGER | Count of files changed |
| **Time** | created_at | TEXT (ISO8601) | Commit creation timestamp |
| **Integrity** | content_hash | TEXT (SHA-256) | Hash of message + file_diffs for dedup |
| **Integrity** | signature | TEXT (nullable) | Optional Ed25519 signature |
| **Metadata** | metadata | JSON (nullable) | Extensible key-value pairs |
| **Audit** | commit_type | TEXT | One of: normal, rollback, squash, merge, system |

**FR-3.2** The `commit_type` field MUST be one of:
- `normal` — Standard micro-commit from agent/user work
- `rollback` — Commit created by a rollback operation (reverting to prior state)
- `squash` — Consolidated commit from squash operation
- `merge` — Commit created by merging branches
- `system` — System-generated commit (init, config change, maintenance)

**FR-3.3** The `file_diffs` JSON array MUST contain entries with:
```json
{
  "path": "internal/store/sqlite/node.go",
  "operation": "modified",
  "old_hash": "abc123...",
  "new_hash": "def456...",
  "additions": 42,
  "deletions": 7
}
```
`operation` is one of: `added`, `modified`, `deleted`, `renamed`.

**FR-3.4** For renamed files, the entry MUST include both `old_path` and `path` (new path), plus a `similarity` percentage.

---

### FR-4: Task-to-Commit Mapping

**FR-4.1** mgit MUST maintain a SQLite database (`.mgit/index.db`) that maps task IDs to commit IDs. This is the **task-commit index** — separate from the go-git object store.

**FR-4.2** The `task_commits` table schema:
```sql
CREATE TABLE task_commits (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL,
    commit_id   TEXT NOT NULL,
    commit_type TEXT NOT NULL DEFAULT 'normal',
    agent_id    TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    message     TEXT NOT NULL,
    files_changed INTEGER NOT NULL DEFAULT 0,
    UNIQUE(task_id, commit_id)
);

CREATE INDEX idx_task_commits_task ON task_commits(task_id);
CREATE INDEX idx_task_commits_commit ON task_commits(commit_id);
CREATE INDEX idx_task_commits_agent ON task_commits(agent_id);
CREATE INDEX idx_task_commits_created ON task_commits(created_at);
```

**FR-4.3** The task-commit mapping MUST support bidirectional queries:
- **Task → Commits:** Given a task ID, return all commits for that task ordered by `created_at`
- **Commit → Task:** Given a commit ID, return the associated task ID
- **Subtree → Commits:** Given a parent task ID (e.g., `PROJ-4.2.1`), return all commits for that task and all descendant tasks (e.g., `PROJ-4.2.1.1`, `PROJ-4.2.1.2`, etc.) using LIKE pattern matching

**FR-4.4** The `task_commits` table is **append-only**. Rows MUST NEVER be deleted or modified. This ensures the audit trail is immutable.

**FR-4.4a** The only exception to FR-4.4 is the `squashed` flag (future enhancement) which marks commits as having been included in a squash operation. This is additive metadata, not a deletion.

**FR-4.5** mgit MUST maintain a `branches` table:
```sql
CREATE TABLE branches (
    name        TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL,
    head_commit TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    squash_commit TEXT,
    UNIQUE(task_id)
);
```

**FR-4.6** mgit MUST use WAL mode for the SQLite index database and dual read/write connection pools, identical to mtix's pattern.

**FR-4.7** mgit MUST maintain a `branch_locks` table for branch-level advisory locking (see NFR-3.5):
```sql
CREATE TABLE branch_locks (
    branch      TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    locked_at   TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
```
This table is used by squash and rollback operations to prevent concurrent modifications to the same branch. Lock timeout is 30 seconds (configurable via `locks.timeout_seconds`). Expired locks are auto-cleaned. See NFR-3.5 for the full two-level locking model.

---

### FR-5: Branch Model

**FR-5.1** mgit MUST automatically create a branch when the first commit is made for a new mtix issue (depth-2 task). The branch name follows the format: `task/{task_id}` (e.g., `task/PROJ-4.2.1`).

**FR-5.1a** Branches are created at the **issue level** (depth 2 in mtix hierarchy). Micro-issues (depth 3+) commit to their parent issue's branch. For example:
- Issue `PROJ-4.2.1` → branch `task/PROJ-4.2.1`
- Micro-issue `PROJ-4.2.1.3` → commits to branch `task/PROJ-4.2.1`
- Micro-micro-issue `PROJ-4.2.1.3.4` → commits to branch `task/PROJ-4.2.1`

**FR-5.2** The `main` branch is the integration branch. Squashed commits are merged to `main` via fast-forward merge.

**FR-5.3** Branch lifecycle:
1. **Created:** When first commit for an issue arrives
2. **Active:** Receiving micro-commits
3. **Squashed:** All micro-commits squashed into single commit on `main`
4. **Archived:** Branch retained for audit but no longer active

**FR-5.4** mgit MUST support listing branches:
- `mgit branch` — list all branches with status
- `mgit branch --active` — list only active branches
- `mgit branch --task PROJ-4.2.1` — show branch for specific task

**FR-5.5** mgit MUST support switching branches:
- `mgit checkout task/PROJ-4.2.1` — switch to task branch
- `mgit checkout main` — switch to main branch

**FR-5.5a** Checkout MUST update the working directory to match the branch tip. If there are uncommitted changes, checkout MUST fail with: `"uncommitted changes exist — commit or stash first"`.

**FR-5.6** mgit MUST detect and warn about orphan branches — branches whose associated mtix task has been cancelled or deleted. `mgit verify` includes orphan branch detection.

---

### FR-6: Rollback & Restore

**FR-6.1** mgit MUST support per-task rollback:
```
mgit rollback --task PROJ-4.2.1.3
```
This restores the working directory to the state **immediately after the last commit of task PROJ-4.2.1.3's predecessor** (i.e., undoes all work done by task PROJ-4.2.1.3 and any subsequent tasks on the same branch).

**FR-6.1a** Per-task rollback operates on the issue branch, not `main`. It affects only the micro-commits for the specified task and any later tasks on the same branch.

**FR-6.2** mgit MUST support point-in-time rollback:
```
mgit rollback --commit <commit_id>
```
This restores the working directory to the exact state at the specified commit.

**FR-6.3** **Append-only rollback:** Rollback MUST NOT delete any commits. Instead, it creates a new commit of type `rollback` that records:
- The target commit being rolled back to
- The commits being "undone" (listed in metadata)
- The reverse diffs that restore the prior state

**FR-6.3a** The rollback commit message format:
```
[MGIT:SYSTEM] Rollback to {target_commit_short_id} (task {task_id})

Reverted commits:
- {commit_1_short_id}: [MGIT:{task_id}] {message}
- {commit_2_short_id}: [MGIT:{task_id}] {message}

Reason: {user_provided_reason or "manual rollback"}
```

**FR-6.4** Rollback MUST be idempotent: rolling back to the same target twice produces the same working directory state. The second rollback creates a no-op rollback commit (empty diff) with `--allow-empty`.

**FR-6.5** mgit MUST support `--dry-run` flag on rollback:
```
mgit rollback --task PROJ-4.2.1.3 --dry-run
```
This shows what commits would be reverted and what files would change, without actually performing the rollback.

**FR-6.6** After a rollback, mgit SHOULD notify mtix (via MCP tool `mtix_reopen` or REST API) to reopen the affected tasks. This is configurable via `mgit.auto_reopen_on_rollback` config key (default: `true`).

**FR-6.7** mgit MUST support `mgit restore <file> --commit <commit_id>` to restore a single file from a specific commit without a full rollback.

---

### FR-7: Squash Workflow

**FR-7.1** mgit MUST support squashing all micro-commits for a task into a single consolidated commit:
```
mgit squash --task PROJ-4.2.1
```

**FR-7.2** The squash operation:
1. Identify all commits on branch `task/PROJ-4.2.1` (for the issue and all its micro-issues)
2. Verify commit chain integrity (no gaps, no corruption)
3. Compute consolidated diff from branch base to branch tip
4. Create a single new commit of type `squash` on the task branch with the consolidated diff
5. If `--to-main` flag is set, fast-forward merge the squash commit to `main` (see requirement-squash-algorithm.md step 10 and requirement-branch-strategy.md §5)
6. Update the branch status to `squashed`
7. Update the task-commit mapping with the new squash commit

**FR-7.3** The squash commit message format:
```
[PROJ-4.2.1] {consolidated_summary}

Squashed from {N} micro-commits:
- {commit_1_short_id}: {message} (task {task_id})
- {commit_2_short_id}: {message} (task {task_id})
- ...

mgit-trace: {branch_name}
mgit-commits: {comma_separated_commit_ids}
```

**FR-7.3a** The `consolidated_summary` is generated by concatenating unique task descriptions, or provided via `--message` flag.

**FR-7.4** Squash MUST be **atomic**: either all micro-commits are squashed successfully, or the operation fails and no changes are made. This is enforced via SQLite transaction wrapping the mapping update and go-git commit creation.

**FR-7.5** mgit MUST support `--to-git` flag to export the squash result as a standard git commit:
```
mgit squash --task PROJ-4.2.1 --to-git
```
This creates a git-format-patch file at `.mgit/exports/{task_id}.patch` that can be applied to the production `.git/` repository via `git am`.

**FR-7.5a** The `--to-git` flag can also directly apply the patch to `.git/` if the user confirms:
```
mgit squash --task PROJ-4.2.1 --to-git --apply
```

**FR-7.6** Squash MUST verify that all micro-commits for the task are present and ordered. If any commit is missing or corrupted, squash MUST fail with a detailed error listing the problem commits.

**FR-7.7** mgit MUST support `--dry-run` flag on squash:
```
mgit squash --task PROJ-4.2.1 --dry-run
```
This shows the consolidated diff and generated commit message without performing the squash.

**FR-7.8** After a successful squash, mgit SHOULD notify mtix (via MCP tool or REST API) that the task's code has been consolidated. This is configurable via `mgit.auto_notify_on_squash` config key (default: `true`).

**FR-7.9** Squash MUST handle the case where some micro-tasks have been rolled back. Only the commits that represent the **current state** (not reverted commits) should be included in the squash.

---

### FR-8: CLI Interface

**FR-8.1** The CLI binary MUST be named `mgit` and be a single static binary (no external dependencies at runtime).

**FR-8.2** All commands MUST support `--json` flag for structured JSON output (for MCP/API integration).

**FR-8.3** All commands MUST return exit code 0 on success and exit code 1 on error.

**FR-8.4** Commands reference:

#### Core Commands

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `mgit init` | Initialize mgit repository | `--link-mtix` (auto-link to .mtix/) |
| `mgit add <files>` | Stage files for commit | `--task <ID>` (stage by task scope), `.` (all), `--all` |
| `mgit commit` | Create micro-commit | `-m <msg>`, `--task <ID>` (required), `--allow-empty` |
| `mgit status` | Show repository status | `--task <ID>` (filter by task), `--short` |
| `mgit log` | Show commit history | `--task <ID>`, `--oneline`, `--graph`, `-n <count>`, `--since`, `--until` |
| `mgit show <commit>` | Show commit details | `--stat` (file stats only), `--format` |
| `mgit diff` | Show changes | `<commit1>..<commit2>`, `--task <ID>`, `--staged`, `--stat` |

#### Workflow Commands

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `mgit rollback` | Revert to prior state | `--task <ID>`, `--commit <hash>`, `--dry-run`, `--reason` |
| `mgit squash` | Consolidate micro-commits | `--task <ID>`, `--to-main`, `--to-git`, `--apply`, `--message`, `--dry-run` |
| `mgit restore <file>` | Restore single file | `--commit <hash>` |
| `mgit cherry-pick <commit>` | Apply specific commit to current branch | `--no-commit` |

#### Branch Commands

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `mgit branch` | List/manage branches | `--active`, `--task <ID>`, `--delete` (only archived) |
| `mgit checkout <branch>` | Switch branches | (fails if uncommitted changes) |
| `mgit merge <branch>` | Merge branch into current | `--squash`, `--no-ff` |

#### Administration Commands

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `mgit verify` | Check repository integrity | `--fix` (attempt auto-repair) |
| `mgit export` | Export repository | `--file <path>`, `--format` (json\|bundle) |
| `mgit import` | Import repository | `--file <path>`, `--merge` |
| `mgit gc` | Run garbage collection | `--aggressive` (repack objects) |
| `mgit config` | Get/set/delete config | `get <key>`, `set <key> <value>`, `delete <key>` |
| `mgit serve` | Start MCP/API server | `--port`, `--mcp-only`, `--api-only` |
| `mgit docs generate` | Generate agent-facing documentation | `--force` (regenerate all, including template files) |
| `mgit audit` | Query audit trail | `--type <type>`, `--task <ID>`, `--since`, `--until` |
| `mgit token` | Manage API tokens | `generate`, `rotate`, `revoke`, `list` |
| `mgit worktree add` | Create linked worktree | `<path>`, `--task <ID>`, `--branch <name>` |
| `mgit worktree list` | List all worktrees | `--porcelain` |
| `mgit worktree remove` | Remove linked worktree | `<path>`, `--force` |
| `mgit worktree prune` | Clean up stale worktree metadata | `--dry-run` |

**FR-8.5** The `mgit log` command MUST support task-filtered output:
```
$ mgit log --task PROJ-4.2.1
commit abc1234 (task/PROJ-4.2.1)
[MGIT:PROJ-4.2.1.4] Complete error handling
Agent: claude-agent-1 | 2026-03-09T14:30:00Z

commit def5678
[MGIT:PROJ-4.2.1.3] Implement validation logic
Agent: claude-agent-1 | 2026-03-09T14:15:00Z

commit 789abcd
[MGIT:PROJ-4.2.1.2] Add unit tests
Agent: claude-agent-1 | 2026-03-09T14:00:00Z

commit bcd1234
[MGIT:PROJ-4.2.1.1] Create store interface
Agent: claude-agent-1 | 2026-03-09T13:45:00Z
```

**FR-8.6** `mgit status` MUST show task-aware status:
```
$ mgit status
On branch task/PROJ-4.2.1 (issue: PROJ-4.2.1)
Active task: PROJ-4.2.1.5

Changes to be committed:
  modified:   internal/store/sqlite/node.go
  added:      internal/store/sqlite/node_test.go

Untracked files:
  internal/store/sqlite/helpers.go

Task commits: 4 (PROJ-4.2.1.1 → PROJ-4.2.1.4)
Branch commits behind main: 0
```

---

### FR-9: REST API

**FR-9.1** mgit MUST expose a REST API when running in server mode (`mgit serve`). The API listens on `api.http_port` (default: `6860`).

**FR-9.2** API endpoints:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check (status, version, uptime) |
| GET | `/api/v1/commits` | List commits with filters (task_id, agent_id, since, until, limit, offset) |
| GET | `/api/v1/commits/{id}` | Get commit details |
| GET | `/api/v1/tasks/{id}/commits` | Get all commits for a task (subtree query) |
| GET | `/api/v1/branches` | List branches with status |
| GET | `/api/v1/branches/{name}` | Get branch details |
| POST | `/api/v1/commits` | Create a commit (for programmatic use) |
| POST | `/api/v1/rollback` | Trigger rollback (body: {task_id or commit_id, reason}) |
| POST | `/api/v1/squash` | Trigger squash (body: {task_id, to_git, message}) |
| GET | `/api/v1/diff` | Get diff between commits or for a task |
| GET | `/api/v1/stats` | Repository statistics |
| GET | `/api/v1/verify` | Run integrity verification |

**FR-9.3** All API responses MUST include:
- `X-Request-ID` header (ULID)
- `Content-Type: application/json`
- Standard error format: `{"error": {"code": "NOT_FOUND", "message": "commit abc123 not found"}}`

**FR-9.4** API error codes map to HTTP status:
- 200: Success
- 201: Created (new commit)
- 400: Invalid input (bad task ID, invalid parameters)
- 404: Not found (commit, task, branch)
- 409: Conflict (uncommitted changes, concurrent write)
- 500: Internal server error

**FR-9.5** The API MUST be localhost-only by default. Non-localhost binding requires explicit `api.bind_address` configuration with a warning log.

---

### FR-10: MCP Server

**FR-10.1** mgit MUST expose an MCP (Model Context Protocol) server for LLM agent integration. The MCP server supports both stdio and SSE transports, identical to mtix's pattern.

**FR-10.1a** Stdio transport: read JSON-RPC from stdin, write to stdout. All logs to file (not stdout).

**FR-10.1b** SSE transport: run on the same HTTP port as the REST API at `/mcp/sse`.

**FR-10.2** MCP tools (15 tools):

| Tool | Parameters | Description |
|------|-----------|-------------|
| `mgit_commit` | task_id (required), message, files (array), allow_empty | Create micro-commit |
| `mgit_add` | files (array), task_id, all | Stage files |
| `mgit_rollback` | task_id or commit_id, reason, dry_run | Rollback to prior state |
| `mgit_squash` | task_id, to_git, apply, message, dry_run | Squash micro-commits |
| `mgit_status` | task_id (optional) | Repository status |
| `mgit_log` | task_id, limit, oneline | Commit history |
| `mgit_diff` | commit1, commit2, task_id, stat | Show changes |
| `mgit_show` | commit_id | Commit details |
| `mgit_branch` | active_only, task_id | List branches |
| `mgit_verify` | fix | Integrity check |
| `mgit_restore` | file, commit_id | Restore single file |
| `mgit_discover` | (none) | List all available tools with descriptions |
| `mgit_worktree_add` | path, task_id, agent_id | Create linked worktree bound to task |
| `mgit_worktree_list` | (none) | List all worktrees with status |
| `mgit_worktree_remove` | path, force | Remove linked worktree |

**FR-10.3** All MCP tool responses MUST be structured JSON (no human-readable formatting needed — MCP is always structured).

**FR-10.4** The MCP server MUST validate all tool inputs before executing. Invalid task IDs, non-existent commits, and missing required parameters MUST return descriptive error messages.

**FR-10.4a** MCP tool input size limits:
- `files` array in `mgit_commit`: maximum **1000 files** per commit
- `message` field: maximum **10,000 characters**
- Maximum concurrent MCP tool invocations: **10** (queued beyond this limit)
- Individual tool execution timeout: **60 seconds**

---

### FR-11: Diff & Comparison

**FR-11.1** mgit MUST support file-level diff:
```
mgit diff                          # unstaged changes vs HEAD
mgit diff --staged                 # staged changes vs HEAD
mgit diff <commit1>..<commit2>     # changes between two commits
```

**FR-11.2** mgit MUST support task-level diff — showing all changes made by a specific task:
```
mgit diff --task PROJ-4.2.1.3
```
This computes the diff from the commit immediately before the task's first commit to the task's last commit.

**FR-11.3** mgit MUST support range diff — showing changes between two task states:
```
mgit diff --task PROJ-4.2.1.3..PROJ-4.2.1.5
```

**FR-11.4** Diff output MUST support multiple formats:
- `--format unified` (default): standard unified diff format
- `--format stat`: file statistics only (files changed, insertions, deletions)
- `--format json`: structured JSON diff for programmatic consumption
- `--format name-only`: just file paths that changed

**FR-11.5** mgit MUST detect binary files and display `"Binary file {path} differs"` instead of attempting text diff.

---

### FR-12: Audit & Integrity

**FR-12.1** mgit MUST maintain an append-only audit log at `.mgit/audit.log`. Every mutation operation (commit, rollback, squash, branch create/delete, config change) MUST be logged with:
```
{ISO8601_timestamp} {operation} {agent_id} {task_id} {commit_id} {details}
```

**FR-12.2** The audit log MUST NEVER be truncated, modified, or deleted. Log rotation creates new files (`audit.log.1`, `audit.log.2`, etc.) with the original preserved.

**FR-12.3** `mgit verify` MUST check:
1. **Object integrity:** Verify SHA-256 hashes of all objects in the go-git store
2. **Chain integrity:** Verify parent-child commit chain is unbroken
3. **Index consistency:** Verify every commit in the go-git store has a corresponding entry in the SQLite task-commit index, and vice versa
4. **Branch consistency:** Verify all branch HEAD refs point to valid commits
5. **Audit log integrity:** Verify audit log is append-only (no gaps in timestamps)

**FR-12.3a** `mgit verify --fix` MUST attempt auto-repair for:
- Missing index entries (rebuild from go-git objects)
- Stale branch refs (update to latest valid commit)
- Missing objects (report, cannot auto-fix)

**FR-12.4** `mgit export` MUST produce a self-contained archive:
```
mgit export --file backup.mgit --format bundle
```
The bundle includes: all objects, refs, index database, audit log, and a manifest with SHA-256 checksums.

**FR-12.5** `mgit import` MUST verify the bundle integrity before importing:
- Verify manifest checksums
- Verify no object corruption
- Support `--merge` mode (add to existing repo) and `--replace` mode (overwrite)

**FR-12.6** mgit MUST perform a startup integrity check on the SQLite index:
- `PRAGMA quick_check`
- Verify schema version matches expected
- Refuse to start if checks fail (with `--skip-integrity-check` override and warning)

---

### FR-13: Configuration

**FR-13.1** mgit configuration is stored in `.mgit/config.yaml` and accessed via dot-notation keys.

**FR-13.2** Configuration schema:

| Key | Default | Description |
|-----|---------|-------------|
| `project.prefix` | (auto-detected from .mtix/) | Project prefix for task IDs |
| `project.name` | (directory name) | Human-readable project name |
| `api.http_port` | 6860 | REST API port |
| `api.bind_address` | 127.0.0.1 | API bind address |
| `mcp.transport` | stdio | MCP transport (stdio or sse) |
| `mcp.log_file` | .mgit/logs/mcp.log | MCP log file path |
| `logging.level` | info | Log level (debug, info, warn, error) |
| `logging.file` | .mgit/logs/mgit.log | Log file path |
| `git.auto_stage` | false | Auto-stage all changes on commit |
| `git.sign_commits` | false | Ed25519 commit signing |
| `git.signing_key` | (none) | Path to Ed25519 private key |
| `squash.auto_notify` | true | Notify mtix on squash completion |
| `squash.message_format` | detailed | Squash message format (detailed, compact) |
| `rollback.auto_reopen` | true | Reopen mtix tasks on rollback |
| `rollback.require_reason` | true | Require reason for rollback |
| `branch.auto_create` | true | Auto-create branch on first task commit |
| `branch.cleanup_on_squash` | false | Archive branch after squash |
| `mtix.api_url` | http://localhost:6851 | mtix REST API URL |
| `mtix.mcp_transport` | stdio | mtix MCP transport for integration |
| `mtix.auto_detect` | true | Auto-detect .mtix/ directory |
| `audit.log_file` | .mgit/audit.log | Audit log file path |
| `audit.max_size_mb` | 100 | Max audit log size before rotation |
| `gc.auto` | true | Auto-GC on pack threshold |
| `gc.pack_threshold` | 1000 | Object count threshold for auto-pack |
| `api.tls_cert` | (none) | Path to TLS certificate file (required for non-localhost) |
| `api.tls_key` | (none) | Path to TLS private key file (required for non-localhost) |
| `api.rate_limit` | 100 | Max requests/second per client IP |
| `api.max_connections` | 50 | Max concurrent API connections |
| `api.request_timeout` | 30 | API request timeout in seconds |
| `api.token_expiry_days` | 90 | API bearer token expiry in days |

**FR-13.3** Configuration changes to `api.*`, `mcp.*`, and `logging.*` keys MUST display a warning: `"Server restart required for this change to take effect."`

**FR-13.4** Invalid configuration keys MUST be rejected with an error listing all valid keys.

---

### FR-14: mtix Integration Protocol

**FR-14.1** mgit MUST integrate with mtix via MCP tools for the primary agent workflow:

```
Agent Workflow:
1. mtix_claim PROJ-4.2.1.3       → Claims task in mtix
2. (agent writes code)
3. mgit_add --all                 → Stages changes
4. mgit_commit --task PROJ-4.2.1.3 -m "Implement feature"  → Micro-commit
5. (agent writes more code)
6. mgit_commit --task PROJ-4.2.1.3 -m "Add tests"          → Another micro-commit
7. mtix_done PROJ-4.2.1.3        → Marks task done in mtix
8. (repeat for other micro-tasks)
9. mgit_squash --task PROJ-4.2.1  → Squash all micro-commits for the issue
10. mgit_squash --to-git --apply  → Apply to production git
11. git push                      → Push to remote (done by human/CI)
```

**FR-14.2** mgit MUST listen for mtix events (when both servers are running) to trigger automatic workflows:
- `task.done` → Log task completion in mgit audit trail
- `task.cancelled` → Archive the task's branch
- `task.rerun` → Create a rollback marker commit
- `task.invalidated` → Flag the task's commits for review in mgit

**FR-14.3** mgit MUST support **cross-tool registration**: when mtix's MCP server starts and detects mgit, it SHOULD register mgit's MCP tools as additional tools available to agents. This is configured via mtix's `mgit.mcp_path` config key pointing to the mgit binary.

**FR-14.4** mgit MUST tag every commit with the mtix session ID (if available) to correlate agent sessions across both systems.

**FR-14.5** mgit MUST support querying mtix for task metadata:
- Task title (for auto-generated commit messages)
- Task status (to prevent commits against done/cancelled tasks)
- Task hierarchy (to determine which branch a micro-commit belongs to)

**FR-14.5a** If mtix is unavailable, mgit MUST operate independently — task IDs are still required but metadata enrichment is skipped. A warning is logged: `"mtix not available — operating in standalone mode"`.

**FR-14.6** mgit MUST expose a `mgit_discover` MCP tool that returns brief descriptions of all mgit tools, enabling agents to discover available operations without loading full schemas.

---

### FR-15: Agent Documentation Generation

> Independent LLM coding agents encountering mgit in a project must be able to learn how to use it without reading developer-facing specs. mgit MUST generate agent-facing documentation — usage guides, skill manifests, CLI references, and workflow patterns — so any agent can onboard itself.

**FR-15.1** The mgit binary MUST generate agent-facing documentation via `mgit docs generate`. This command MUST produce a complete set of markdown files that LLM agents can read to understand how to use mgit. The generated docs MUST reflect the actual CLI commands, flags, MCP tools, and configuration of the running version — they MUST NOT go stale.

**FR-15.2** `mgit docs generate` MUST produce the following files in the project's `docs/` directory (alongside the `.mgit/` directory). `mgit init` MUST add `docs/` to `.gitignore` if a `.gitignore` file exists, since these files are auto-generated and should not cause merge conflicts in version control.

| File | Purpose | Generation Method |
|------|---------|-------------------|
| `AGENTS.md` | Entry point for any AI agent using mgit. Rules, commit workflow, task-tagging conventions, squash patterns, rollback semantics, do's/don'ts. | Template + auto-populated project config |
| `CLAUDE.md` | Claude-specific instructions (imports AGENTS.md, adds Claude-specific conventions for Claude Code / Cowork) | Template |
| `SKILL.md` | Skill manifest with YAML frontmatter (name, description, allowed-tools, version) for Claude Code / Cowork / MCP discovery | Auto-generated from binary version + config |
| `CLI_REFERENCE.md` | Complete command reference — every command, flag, and output format for all 15+ commands | Auto-generated from Cobra command tree |
| `MCP_TOOLS.md` | Reference for all 15 MCP tools with parameters, return types, examples | Auto-generated from MCP tool registry |
| `WORKFLOWS.md` | Step-by-step workflows: micro-commit → accumulate → squash → export, branch lifecycle, rollback patterns | Template + project prefix |
| `ROLLBACK_GUIDE.md` | How agents should handle rollbacks, what happens to mtix task status, dry-run workflow, conflict resolution | Template |
| `SQUASH_GUIDE.md` | When to squash, how squash works, --to-git export, verifying squash integrity | Template |
| `TROUBLESHOOTING.md` | Common errors (ErrChainBroken, ErrRollbackConflict, ErrSquashFailed, ErrLockContention), resolution steps | Template + auto-generated error codes |

**FR-15.3** Auto-generated files (CLI_REFERENCE.md, MCP_TOOLS.md, SKILL.md) MUST be regenerated on every `mgit docs generate` invocation. Template-based files MUST only be generated if they don't already exist (to preserve human edits). A `--force` flag MUST regenerate all files.

**FR-15.3a** Auto-generated sections within template files MUST be delimited by markers (e.g., `<!-- AUTO-GENERATED: CLI_COMMANDS -->` ... `<!-- END AUTO-GENERATED -->`). These sections are regenerated on every `mgit docs generate` invocation even in existing files. Human edits outside these markers are preserved. This ensures that when mgit adds new commands or MCP tools, existing human-edited docs get the updates without losing customizations.

**FR-15.4** `mgit init` MUST automatically run `mgit docs generate` as part of repository initialization, so agent docs are available from day 1.

**FR-15.5** The AGENTS.md template MUST include:
- Project prefix and current configuration
- The micro-commit workflow (claim task → write code → mgit commit → repeat → squash → export)
- Commit message conventions and task-tagging format (`[MGIT:{TASK_ID}] {message}`)
- Branch model: which branch to commit to, when branches are auto-created
- Squash workflow: when to squash, what happens to micro-commits, how to export to git
- Rollback rules: append-only semantics, how rollback affects mtix task status
- Security warnings: never delete commits, never force-push, audit trail is permanent
- Explicit rules: "Always tag commits with task ID", "Never commit to main directly", "Use dry-run before rollback"

**FR-15.6** The SKILL.md MUST contain YAML frontmatter compatible with Claude Code / Cowork skill discovery:

```yaml
---
name: mgit
description: Micro version control for LLM coding agents. Task-tagged micro-commits, per-task rollback, squash-to-git workflow.
version: {auto-detected from binary}
allowed-tools:
  - mgit_commit
  - mgit_add
  - mgit_log
  - mgit_diff
  - mgit_status
  - mgit_rollback
  - mgit_squash
  - mgit_branch
  - mgit_show
  - mgit_verify
  - mgit_export
  - mgit_discover
---
```

**FR-15.7** `mgit docs generate` MUST also be available as an MCP tool (`mgit_docs_generate`) so agents can regenerate docs if they detect they're outdated. This tool is in addition to the 15 core MCP tools defined in FR-10 (total: 16 MCP tools).

**FR-15.8** The CLI_REFERENCE.md MUST be auto-generated from the Cobra command tree. For each command, include: command name, description, all flags with types and defaults, usage examples, and exit codes. The format MUST be machine-readable (agents should be able to parse it) while remaining human-readable.

**FR-15.9** The MCP_TOOLS.md MUST be auto-generated from the MCP tool registry. For each tool, include: tool name, description, input parameters (name, type, required/optional, default), return schema, example request/response, and error codes.

**FR-15.10** `mgit docs generate` MUST produce a **Requirements Traceability Matrix (RTM)** at `docs/TRACEABILITY_MATRIX.md`. The RTM provides bidirectional traceability required by DO-178C (OBJ-5) and MIL-STD-498:

| Column | Source | Description |
|--------|--------|-------------|
| Requirement ID | REQUIREMENTS.md FR/NFR numbers | The requirement being traced |
| Design Reference | CODING-STYLE.md section or file path | Where the design is documented |
| Code Location | Godoc `Refs:` annotations | Which functions implement it |
| Test Functions | Test function names matching `Test*` | Which tests verify it |
| Verification Status | Auto-computed | Pass/Fail/Not-Yet-Implemented |

The RTM is auto-generated by scanning:
1. REQUIREMENTS.md for all `FR-x.y` and `NFR-x.y` identifiers
2. Go source files for `Refs: FR-x.y` in Godoc comments
3. Test files for test functions whose comments reference FR/NFR numbers
4. mgit-tasks.json for task → requirement mappings

The RTM MUST flag any requirement with zero code references or zero test references as "UNTESTED" — this is a certification blocker for DO-178C and MIL-STD-498.

---

### FR-16: Agent Worktrees (Multi-Agent Parallel Development)

**FR-16.1** mgit MUST support multiple linked worktrees, allowing parallel LLM coding agents to work on different tasks simultaneously within the same mgit repository.

**FR-16.2** The CLI surface MUST mirror standard git worktree semantics so that LLM agents trained on git documentation can use mgit worktrees without learning new abstractions:

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `mgit worktree add <path>` | Create linked worktree | `--task <ID>` (required), `--branch <name>` |
| `mgit worktree list` | List all worktrees | `--porcelain` |
| `mgit worktree remove <path>` | Remove linked worktree | `--force` |
| `mgit worktree prune` | Clean up stale worktree metadata | `--dry-run` |

**FR-16.3** Every worktree MUST be bound to exactly one mtix task ID at creation time via the `--task` flag. This binding is recorded in `index.db` and enforced throughout the worktree's lifecycle:
- Commits made from within a worktree are automatically tagged with the bound task ID
- The agent does not need to pass `--task` on every `mgit commit` inside a worktree — the binding is implicit
- Attempting to commit with a different `--task` than the worktree binding MUST return `ErrTaskMismatch`

**FR-16.4** The `.mgit/` directory structure for worktrees:
```
.mgit/
├── worktrees/            # Linked worktree metadata
│   ├── <worktree-name>/
│   │   ├── HEAD          # Branch reference for this worktree
│   │   ├── task_binding  # Plain text file: mtix task ID
│   │   ├── agent_id      # Plain text file: agent identifier
│   │   ├── created_at    # ISO-8601 creation timestamp
│   │   └── locked        # Lock file (present if worktree is locked)
│   └── ...
├── objects/              # Shared object store (all worktrees)
├── refs/                 # Shared branch references
├── index.db              # Shared task-commit index + worktree registry
└── ...
```

**FR-16.5** Worktrees MUST share the mgit object store and SQLite index. All commits from any worktree are visible in `mgit log` from any other worktree. Branch references are shared — a branch created in one worktree is visible from all others.

**FR-16.6** Worktree isolation rules:
- Two worktrees MUST NOT have the same branch checked out simultaneously. Attempting to check out a branch that is active in another worktree MUST return `ErrBranchInUse`
- Two worktrees MUST NOT be bound to the same task ID. Attempting to create a worktree for a task that already has an active worktree MUST return `ErrTaskAlreadyBound`
- Each worktree has its own working directory and index — changes in one worktree do not affect another's working state

**FR-16.7** Worktree lifecycle integration with task workflow:
- When a task is squashed (`mgit squash --task <ID>`), the corresponding worktree SHOULD be automatically removed (with a confirmation prompt on CLI, automatic on MCP)
- When a task is rolled back (`mgit rollback --task <ID>`), the worktree's working directory MUST be updated to reflect the rolled-back state
- Stale worktrees (no commits for a configurable period, default 24 hours) MUST be detectable via `mgit worktree prune --dry-run`

**FR-16.8** MCP tools for worktree management:

| Tool | Parameters | Returns |
|------|-----------|---------|
| `mgit_worktree_add` | `path` (string, required), `task_id` (string, required), `agent_id` (string) | `{path, branch, task_id, created_at}` |
| `mgit_worktree_list` | None | `[{path, branch, task_id, agent_id, head_commit, created_at}]` |
| `mgit_worktree_remove` | `path` (string, required), `force` (boolean) | `{removed: true}` |

**FR-16.9** Worktree-aware commands. When mgit detects it is running inside a linked worktree:
- `mgit status` shows the worktree's task binding in the header: `On worktree: <path> (task: PROJ-4.2.1)`
- `mgit commit` uses the worktree's bound task ID if `--task` is not explicitly provided
- `mgit branch` marks the worktree's current branch with `+` (following git convention for worktree branches)
- `mgit log --task <ID>` works from any worktree, not just the one bound to that task

**FR-16.10** Pluggable worktree backend. The worktree subsystem MUST be implemented behind an interface:

```go
// WorktreeManager defines the contract for worktree lifecycle operations.
// The default implementation (v1) manages worktrees at the application level.
// Future implementations may delegate to go-git v6's native worktree support.
type WorktreeManager interface {
    Add(ctx context.Context, opts WorktreeAddOptions) (*WorktreeInfo, error)
    List(ctx context.Context) ([]WorktreeInfo, error)
    Remove(ctx context.Context, path string, force bool) error
    Prune(ctx context.Context, dryRun bool) ([]string, error)
    Resolve(ctx context.Context, path string) (*WorktreeInfo, error)
}
```

The v1 implementation manages worktrees at the mgit application level using the `.mgit/worktrees/` directory structure defined in FR-16.4. The interface MUST be designed so that a future go-git v6 native implementation can replace it without changes to CLI, MCP, or service layer code (see ADR-004).

**FR-16.11** Concurrent worktree safety:
- Worktree creation and removal MUST be serialized through SQLite transactions (same write-connection model as FR-4, NFR-2)
- The worktree registry in `index.db` MUST include:

```sql
CREATE TABLE worktrees (
    path TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    branch TEXT NOT NULL,
    task_id TEXT NOT NULL UNIQUE,
    agent_id TEXT,
    created_at TEXT NOT NULL,
    last_commit_at TEXT
);
```

- Commits from concurrent worktrees are safe because they operate on different branches — go-git's loose object writes use atomic file renames

**FR-16.12** The main worktree (the project root directory) is always present and does not require `mgit worktree add`. It behaves as a standard mgit working directory with no implicit task binding. The `--task` flag on `mgit commit` is required when committing from the main worktree (existing behavior).

---

## 3. Non-Functional Requirements

### NFR-1: Performance

**NFR-1.1** Single micro-commit creation: **<5ms** (excluding I/O to disk). This includes: staging verification, commit object creation, tree hash computation, index update.

**NFR-1.2** Commit log query by task ID: **<50ms** for repositories with up to 10,000 commits.

**NFR-1.3** Squash operation for 100 micro-commits: **<500ms**. This includes: commit collection, diff consolidation, new commit creation, index update.

**NFR-1.4** File diff computation: **<100ms** for changesets affecting up to 1,000 files.

**NFR-1.5** Repository integrity verification: **<1s** for repositories with up to 10,000 commits.

**NFR-1.6** CLI startup time (including repository detection and index open): **<50ms**.

**NFR-1.7** Task-to-commit subtree query (all commits for a task and descendants): **<100ms** for task hierarchies up to depth 10.

---

### NFR-2: Storage

**NFR-2.1** mgit MUST use go-git's object storage model:
- **Blobs:** File content, content-addressed by SHA-256
- **Trees:** Directory listings, mapping names to blob/tree references
- **Commits:** Metadata + tree reference + parent reference(s)
- **Tags:** Named references to commits (for milestones)

**NFR-2.2** go-git MUST be configured for filesystem storage (not in-memory). Objects stored in `.mgit/objects/`.

**NFR-2.3** Packfile compression: Objects MUST be packed into packfiles when the loose object count exceeds `gc.pack_threshold` (default: 1000). This is handled by `mgit gc` or auto-gc.

**NFR-2.4** The SQLite task-commit index MUST use:
- WAL mode for concurrent read/write
- Dual connection pools (1 writer, N readers)
- Foreign key enforcement enabled
- Temp file test stores (not `:memory:`) for dual-pool support in tests

**NFR-2.5** Estimated storage overhead per micro-commit: ~200 bytes metadata + actual file diffs (content-addressed, deduplicated via go-git).

---

### NFR-3: Reliability

**NFR-3.1** mgit MUST guarantee **append-only** semantics:
- No commit is ever deleted from the object store
- No row is ever deleted from the task-commit index
- Rollback creates new commits, not deletions
- Squash creates new commits and marks old ones (additive metadata)

**NFR-3.2** All write operations MUST be **fsync'd** to disk before returning success. This ensures crash recovery — no commit is lost due to an OS crash or power failure.

**NFR-3.3** The SQLite index MUST use `journal_mode=WAL` and `synchronous=FULL` for crash safety. While `synchronous=NORMAL` offers better performance, safety-critical deployments (hospitals, airlines, DoD) require `FULL` to guarantee that committed data survives power loss. The performance cost (~10-20% slower writes) is acceptable given mgit's <5ms commit target.

**NFR-3.3a** The go-git object store wrapper MUST explicitly call `fsync()` (via `os.File.Sync()`) after writing commit objects, tree objects, and reference updates. go-git's default filesystem storage does not guarantee fsync. mgit MUST wrap the go-git `Storer` interface with a syncing decorator that calls `Sync()` on the underlying file after every `SetEncodedObject` and `SetReference` call. This ensures that a power failure between the SQLite index write and the go-git object write cannot leave the repository in an inconsistent state.

**NFR-3.4** Squash operations MUST be **atomic**: either the squash completes fully (new commit + index updates + branch status) or no changes are made. This is enforced via SQLite transactions spanning all index mutations.

**NFR-3.5** mgit MUST handle concurrent access using a two-level locking model:

**Level 1: Process-level PID lock** (`.mgit/locks/mgit.lock`):
- Prevents multiple mgit processes from writing simultaneously
- Contains PID + timestamp; stale locks auto-cleaned on startup (FR-1.4)
- All write operations acquire this lock; read operations do not

**Level 2: Branch-level advisory locks** (SQLite `branch_locks` table):
- Prevents concurrent squash/rollback on the same branch
- Schema: `CREATE TABLE branch_locks (branch TEXT PRIMARY KEY, agent_id TEXT NOT NULL, locked_at TEXT NOT NULL, expires_at TEXT NOT NULL)`
- Lock timeout: 30 seconds (configurable via `locks.timeout_seconds`)
- If lock holder crashes, lock auto-expires after timeout
- PID lock and branch locks are independent: PID lock is for process mutual exclusion, branch locks are for operation coordination within a single process or across MCP tool invocations

Read operations are always safe (WAL mode). If a write fails due to PID lock contention, return `ErrLockContention` with advisory: `"another mgit process is writing — retry in a moment"`. If a branch lock fails, return `ErrBranchLocked` with the locking agent_id and expiry time.

**NFR-3.6** On startup, mgit MUST:
1. Check for stale PID lock (process not running) and clean up
2. Run `PRAGMA quick_check` on index database
3. Verify HEAD ref points to a valid commit
4. Log startup integrity check result

---

### NFR-4: Tech Stack

**NFR-4.1** Language: **Go 1.23+** (same as mtix)

**NFR-4.2** Git engine: **go-git v5** (`github.com/go-git/go-git/v5`) — pure Go implementation, no CGO, no external git binary dependency.

**NFR-4.3** Index database: **modernc.org/sqlite** — pure Go SQLite driver, no CGO (same as mtix).

**NFR-4.4** CLI framework: **spf13/cobra** (same as mtix).

**NFR-4.5** REST API: **labstack/echo/v4** (same as mtix).

**NFR-4.6** MCP: **github.com/mark3labs/mcp-go** (same as mtix).

**NFR-4.7** Build: Single static binary via `go build`. No runtime dependencies. Cross-compilation supported.

**NFR-4.8** The mgit binary MUST be fully independent — it does NOT require mtix to be installed. mtix integration is optional and gracefully degrades when mtix is unavailable.

---

### NFR-5: Security

**NFR-5.1** mgit uses a **dual-hash model** due to git protocol constraints:

- **Git object IDs** (commit hash, tree hash, blob hash) use **SHA-1** per the git wire protocol. This is inherent to go-git v5 and the git specification. SHA-256 object IDs are experimental in git and not supported by go-git in production.
- **mgit content hashes** (`content_hash` field in commit data model FR-3.1) use **SHA-256** (NIST FIPS 180-4) for integrity verification. This is mgit's own cryptographic guarantee, independent of git object addressing.
- **Audit log integrity** checks use SHA-256.

The `content_hash` (SHA-256) is the authoritative integrity field for mgit operations. Git SHA-1 object IDs provide structural addressing within the go-git object store. No MD5 fallback is permitted for either purpose. See ADR-002 for the full rationale.

**NFR-5.1a** `mgit verify` MUST verify both hash types: SHA-1 for go-git object integrity (recompute from object content) and SHA-256 for mgit content integrity (recompute from commit message + file_diffs).

**NFR-5.2** Optional commit signing via Ed25519 keys. When `git.sign_commits=true`, every commit includes a signature that can be verified.

**NFR-5.3** The REST API MUST bind to localhost (127.0.0.1) by default. Non-localhost binding requires explicit configuration and logs a warning.

**NFR-5.4** All SQL queries MUST use parameterized statements. String interpolation in SQL is forbidden.

**NFR-5.5** The audit log MUST be append-only at the filesystem level. mgit opens the audit log in append mode (`O_APPEND`) and never seeks or truncates.

**NFR-5.6** File paths in commits MUST be validated:
- No path traversal (`../`)
- No absolute paths (must be relative to project root)
- No null bytes
- Maximum path length: 4096 characters

**NFR-5.7** The MCP server MUST validate all tool inputs against their schemas before execution. Malformed JSON or missing required fields MUST be rejected with descriptive errors.

**NFR-5.8** mgit MUST sanitize all user-provided strings in commit messages and metadata to prevent injection attacks in downstream systems (e.g., shell injection via commit messages used in scripts).

**NFR-5.9** When `api.bind_address` is not `127.0.0.1` or `::1`, TLS MUST be enabled. mgit MUST refuse to start with non-localhost binding unless `api.tls_cert` and `api.tls_key` configuration keys are set and point to valid certificate and key files. This prevents accidental plaintext API exposure in hospital SOCs, DoD SCIFs, or multi-host environments.

**NFR-5.10** The REST API MUST enforce rate limiting and request size constraints:
- Maximum request body size: **10MB** (prevents memory exhaustion from oversized commits)
- Rate limit: **100 requests/second** per client IP (configurable via `api.rate_limit`)
- Maximum concurrent connections: **50** (configurable via `api.max_connections`)
- Request timeout: **30 seconds** (configurable via `api.request_timeout`)

**NFR-5.11** API authentication token lifecycle:
- Tokens MUST be generated via `mgit token generate` (cryptographically random, 32 bytes, base64-encoded)
- Tokens MUST have a configurable expiry (default: 90 days, configurable via `api.token_expiry_days`)
- Expired tokens MUST be rejected with HTTP 401 and descriptive error
- Failed authentication attempts MUST be rate-limited: **5 failures per minute** per client IP, then 60-second lockout
- Token rotation: `mgit token rotate` generates a new token and invalidates the previous one
- Token revocation: `mgit token revoke` invalidates the current token immediately
- Token listing: `mgit token list` shows active tokens with masked values (e.g., `****...abcd`) and expiry dates
- Token storage: tokens stored in a **separate file** `.mgit/tokens.json` with file permissions `0600`. Tokens MUST NOT be stored in `config.yaml` to prevent accidental exposure through config sharing, logging, or `mgit config` output. The `tokens.json` file contains: `{"tokens": [{"hash": "sha256-of-token", "created_at": "ISO8601", "expires_at": "ISO8601", "revoked": false}]}`. Only the token hash is stored; the plaintext token is displayed once at generation time

---

## 4. LLM Integration Patterns

### Pattern 1: Single Agent, Sequential Tasks

```
1. Agent claims micro-issue PROJ-4.2.1.1
2. Agent writes code
3. Agent calls mgit_commit --task PROJ-4.2.1.1 -m "Implement feature"
4. Agent claims micro-issue PROJ-4.2.1.2
5. Agent writes tests
6. Agent calls mgit_commit --task PROJ-4.2.1.2 -m "Add tests"
7. (repeat for all micro-issues)
8. Human reviews via mgit log --task PROJ-4.2.1
9. Human approves → mgit squash --task PROJ-4.2.1 --to-git --apply
```

### Pattern 2: Multi-Agent, Parallel Tasks

```
Agent A (branch task/PROJ-4.2.1):
  1. Commits PROJ-4.2.1.1, PROJ-4.2.1.2

Agent B (branch task/PROJ-4.2.2):
  1. Commits PROJ-4.2.2.1, PROJ-4.2.2.2

Both agents work in parallel on separate branches.
Squash happens independently per issue.
No merge conflicts between branches.
```

### Pattern 3: Rollback and Rework

```
1. Agent completes PROJ-4.2.1.1 through PROJ-4.2.1.4
2. Human review catches issue at PROJ-4.2.1.3
3. Human: mgit rollback --task PROJ-4.2.1.3 --reason "incorrect approach"
4. mgit notifies mtix → tasks PROJ-4.2.1.3 and PROJ-4.2.1.4 reopened
5. Agent re-claims PROJ-4.2.1.3 with new approach
6. Agent commits new PROJ-4.2.1.3 and PROJ-4.2.1.4
7. Branch now has: 1.1, 1.2, 1.3(old), 1.4(old), ROLLBACK, 1.3(new), 1.4(new)
8. Squash consolidates only the current state (old reverted commits excluded)
```

---

## 5. Project Structure

```
mgit/
├── cmd/
│   └── mgit/
│       ├── main.go           # Entry point with Cobra root command
│       ├── init.go           # mgit init
│       ├── commit.go         # mgit commit
│       ├── add.go            # mgit add
│       ├── log.go            # mgit log
│       ├── show.go           # mgit show
│       ├── diff.go           # mgit diff
│       ├── rollback.go       # mgit rollback
│       ├── squash.go         # mgit squash
│       ├── restore.go        # mgit restore
│       ├── branch.go         # mgit branch
│       ├── checkout.go       # mgit checkout
│       ├── cherry_pick.go    # mgit cherry-pick
│       ├── merge.go          # mgit merge
│       ├── status.go         # mgit status
│       ├── verify.go         # mgit verify
│       ├── export.go         # mgit export
│       ├── import.go         # mgit import
│       ├── gc.go             # mgit gc
│       ├── config.go         # mgit config
│       ├── serve.go          # mgit serve
│       ├── token.go          # mgit token (generate, rotate, revoke, list)
│       ├── worktree.go       # mgit worktree (add, list, remove, prune)
│       └── workflow.go       # Shared workflow helpers
├── internal/
│   ├── model/
│   │   ├── commit.go         # Commit struct and validation
│   │   ├── branch.go         # Branch struct
│   │   ├── diff.go           # FileDiff struct
│   │   ├── task.go           # TaskID parsing and validation
│   │   ├── worktree.go       # WorktreeInfo struct and WorktreeManager interface
│   │   └── errors.go         # Sentinel errors
│   ├── store/
│   │   ├── git/
│   │   │   ├── repository.go # go-git wrapper
│   │   │   ├── commit.go     # Commit operations
│   │   │   ├── tree.go       # Tree operations
│   │   │   ├── branch.go     # Branch operations
│   │   │   ├── diff.go       # Diff operations
│   │   │   └── object.go     # Object store operations
│   │   └── index/
│   │       ├── schema.go     # SQLite DDL
│   │       ├── store.go      # Index store interface + implementation
│   │       ├── task_commits.go # Task-commit mapping CRUD
│   │       ├── branches.go   # Branch CRUD
│   │       ├── worktrees.go  # Worktree registry CRUD
│   │       └── migration.go  # Schema migration runner
│   ├── worktree/
│   │   ├── manager.go        # Default WorktreeManager implementation (v1)
│   │   └── manager_test.go   # Worktree manager tests
│   ├── service/
│   │   ├── commit_service.go # Commit orchestration
│   │   ├── squash_service.go # Squash logic
│   │   ├── rollback_service.go # Rollback logic
│   │   ├── branch_service.go # Branch management
│   │   ├── worktree_service.go # Worktree lifecycle orchestration
│   │   ├── diff_service.go   # Diff computation
│   │   ├── verify_service.go # Integrity verification
│   │   ├── audit_service.go  # Audit log management
│   │   └── config_service.go # Configuration management
│   ├── api/
│   │   └── http/
│   │       ├── server.go     # Echo server setup
│   │       ├── handlers.go   # REST API handlers
│   │       └── middleware.go  # Request ID, logging, CORS
│   ├── mcp/
│   │   ├── server.go         # MCP server (stdio + SSE)
│   │   └── tools.go          # 15 core MCP tool definitions (+ mgit_docs_generate = 16 total)
│   ├── mtix/
│   │   ├── client.go         # mtix MCP/REST client
│   │   └── events.go         # mtix event listener
│   └── testutil/
│       ├── store.go          # Test store helpers
│       ├── commit.go         # Test commit helpers
│       └── fixtures.go       # Test data fixtures
├── e2e/
│   ├── workflow_test.go      # E2E agent workflow tests
│   └── rollback_test.go      # E2E rollback/squash tests
├── go.mod
├── go.sum
├── Makefile
├── .golangci.yml
├── .goreleaser.yml
├── internal/docs/
│   ├── generator.go         # mgit docs generate — auto-doc engine
│   └── templates/
│       ├── agents.md.tmpl
│       ├── claude.md.tmpl
│       ├── skill.md.tmpl
│       ├── workflows.md.tmpl
│       ├── rollback_guide.md.tmpl
│       ├── squash_guide.md.tmpl
│       └── troubleshooting.md.tmpl
└── docs/                     # Auto-generated agent documentation output (FR-15)
    ├── AGENTS.md             # Generated: agent quickstart guide
    ├── CLAUDE.md             # Generated: Claude Code / Cowork instructions
    ├── SKILL.md              # Generated: YAML frontmatter + capabilities
    ├── CLI_REFERENCE.md      # Generated: full command reference from Cobra tree
    ├── MCP_TOOLS.md          # Generated: MCP tool reference
    ├── WORKFLOWS.md          # Generated: commit → squash → export patterns
    ├── ROLLBACK_GUIDE.md     # Generated: rollback semantics for agents
    ├── SQUASH_GUIDE.md       # Generated: squash workflow for agents
    └── TROUBLESHOOTING.md    # Generated: errors, resolution steps
```

---

## 6. Glossary

| Term | Definition |
|------|-----------|
| **Micro-commit** | A commit in mgit tagged with an mtix task ID, representing work on a single micro-issue |
| **Task branch** | A branch in mgit corresponding to an mtix issue (depth-2 task), containing all micro-commits for that issue's micro-issues |
| **Squash** | The operation of consolidating multiple micro-commits into a single commit |
| **Rollback** | Reverting the working directory to a prior state by creating a new revert commit (append-only) |
| **Task-commit index** | The SQLite database mapping mtix task IDs to mgit commit IDs |
| **Object store** | The go-git storage of blobs, trees, and commits (structurally addressed by SHA-1 per git protocol; integrity verified by SHA-256 content_hash per ADR-002) |
| **Append-only** | A constraint ensuring data is only added, never modified or deleted |
| **Linked worktree** | An additional working directory attached to the mgit repository, bound to a single mtix task, sharing the object store and index with the main worktree and all other linked worktrees |
| **Main worktree** | The primary working directory at the project root; always present, not bound to a specific task |
| **WorktreeManager** | Pluggable interface for worktree lifecycle operations; v1 is mgit-managed, future versions may delegate to go-git v6 native worktree support |
| **Content hash** | SHA-256 hash of commit content used for deduplication and integrity verification |
