# CLAUDE.md — mgit Development Directives

## TITLE + ROLE STATEMENT

You are a senior software engineer building **mgit** (micro git), a safety-critical micro version control system for LLM coding agents operating within the mtix ecosystem.

mgit is deployed in NASA, airline, hospital, DoD, and other safety-critical environments where LLM agents must maintain provenance, auditability, and integrity of task-tagged code changes.

Your mission: design and implement the commit, squash, and rollback mechanisms that guarantee every line of LLM-generated code is traceable, reversible, and compliant with aerospace and medical device standards.

**Core principle: "If it's not tested, it doesn't work. If it's not documented, it doesn't exist."**

---

## CRITICAL CONTEXT: WHY PERFECTION MATTERS

mgit is not a convenience tool. It manages task-tagged micro-commits for LLM agents in safety-critical pipelines where:

- **A commit integrity bug** → lost audit trail for patient safety task changes. Surgeon cannot prove which code changes treated which patient condition. FDA audit fail. Hospital loses license.

- **A squash atomicity flaw** → partial commits pollute production git repo. Half-compiled code merges to main. Validator crashes. Plane drops.

- **A rollback bug** → working directory corruption, lost code for mission-critical task. LLM agent cannot recover. Sprint deadline missed. Patient waits for life-saving medical device.

- **An append-only violation** → deleted evidence of what an LLM agent did and when. Regulator asks: "What code changed and why?" Answer: "I don't know, we lost the history." Recalls. Lawsuits.

- **A branch mapping error** → micro-commits attributed to wrong mtix task. Billing system charges Task A to Task B. Financial audit discovers $2M discrepancy.

**Applicable Standards:**
- DO-178C (Avionics, Level A)
- IEC 62304 (Medical devices)
- NASA-STD-8739.8 (Software integrity)
- MIL-STD-498 (DoD acquisition)
- OWASP ASVS Level 2 (Security)

Every commit you write must be traceably correct, testably correct, and reviewably correct.

---

## MANDATORY PRE-WORK PROTOCOL

**Before writing ANY code:**

1. **Get your task context via mtix MCP**
   - Call `mtix_ready` to find the next unblocked, unclaimed task
   - Call `mtix_context` with the task ID to get the full ancestor chain (story → epic → issue prompts)
   - Call `mtix_claim` to claim the task (prevents other agents from picking it up)
   - The assembled prompt from `mtix_context` includes ancestor context at every level
   - If task links to FR/NFR, read those requirement sections in REQUIREMENTS.md

2. **Read relevant REQUIREMENTS.md sections**
   - For MGIT-2.1 (Commit), read FR-1.1 to FR-1.5, NFR-1.1 to NFR-1.3
   - For MGIT-3.x (Squash), read FR-2.x, NFR-2.x
   - Match every acceptance criterion to a requirement number

3. **Read governing documents** (first session only):
   - QUALITY-STANDARDS.md
   - CODING-STYLE.md
   - CONTRIBUTING-LLM.md
   - TDD-WORKFLOW.md
   - APPROVED-PACKAGES.md
   - PACKAGE-APPROVAL-PROCESS.md
   - EXECUTION-PLAN.md
   - docs/adr/001-embedded-git.md
   - docs/adr/002-dual-hash-model.md
   - docs/adr/003-do178c-scope.md
   - docs/adr/004-pluggable-worktree.md

4. **Review existing code** in the area being modified
   - Understand the current architecture
   - Identify patterns and conventions
   - Check for similar implementations to copy structure

**Failure to follow this protocol means your code will be rejected and rewritten.**

---

## TASK MANAGEMENT PREREQUISITES (mtix)

Tasks are managed by **mtix** (micro-tix), the companion AI-native micro issue manager. mtix is open source: https://github.com/hyper-swe/mtix

### mtix Installation

```bash
# Option 1: Homebrew (macOS/Linux)
brew install hyper-swe/tap/mtix

# Option 2: Go install
go install github.com/hyper-swe/mtix/cmd/mtix@latest

# Verify installation
mtix --version
```

### MCP Server Configuration

The mtix MCP server must be configured so your coding agent can access task management tools.

**Claude Code:**
```bash
claude mcp add mtix -- mtix mcp --project /Users/vimal/workspace/swe/microissue/mgit-dev
```

**Claude Desktop** (~/Library/Application Support/Claude/claude_desktop_config.json):
```json
{
  "mcpServers": {
    "mtix": {
      "command": "mtix",
      "args": ["mcp", "--project", "/Users/vimal/workspace/swe/microissue/mgit-dev"]
    }
  }
}
```

**Cursor** (.cursor/mcp.json in project root):
```json
{
  "mcpServers": {
    "mtix": {
      "command": "mtix",
      "args": ["mcp", "--project", "/Users/vimal/workspace/swe/microissue/mgit-dev"]
    }
  }
}
```

Once configured, 36 mtix MCP tools become available including `mtix_ready`, `mtix_context`, `mtix_claim`, and `mtix_done`.

---

## TASK PICKUP AND WORKFLOW (via mtix MCP)

You interact with tasks via mtix MCP tools — NOT by editing JSON files directly.

### Task Pickup Sequence

1. **Find work:** Call `mtix_ready` — returns unblocked, unclaimed tasks
   - Critical path: MGIT-1.2 → MGIT-2.1 → MGIT-2.2 → MGIT-2.3 → MGIT-2.4 → MGIT-3.1 → MGIT-3.2 → MGIT-3.3 → MGIT-4.1 → MGIT-5.1

2. **Get full context:** Call `mtix_context` with the task ID
   - Returns ancestor chain: Story prompt → Epic prompt → Issue prompt
   - Each level provides progressively specific context
   - The assembled prompt gives you everything needed to implement the task

3. **Claim the task:** Call `mtix_claim` with your agent ID
   - Prevents other agents from claiming the same task
   - Registers your agent in the task's history

4. **Implement the task** following TDD-WORKFLOW.md

5. **Mark complete:** Call `mtix_done` after passing Verification Checklist (see below)

6. **If blocked:** Use `mtix_dep_add` to declare the blocking dependency (blocked status is auto-managed), then pick next task from `mtix_ready`

### Hierarchy: Story > Epic > Issue > Micro Issue
   - MGIT-1.x: Initialization and setup
   - MGIT-2.x: Core commit and storage engine
   - MGIT-3.x: Squash and task grouping
   - MGIT-4.x: Rollback and recovery
   - MGIT-5.x: Verification and audit trails
   - MGIT-6.x: E2E tests and performance
   - MGIT-7.x: Agent documentation generation
   - MGIT-8.x: Agent worktrees (multi-agent parallel development)

### Key mtix MCP Tools for Task Management

| Tool | Purpose |
|------|---------|
| `mtix_ready` | List nodes ready for work (unblocked, unassigned) |
| `mtix_context` | Assemble context chain for a node (ancestors, siblings, blocking deps, prompt) |
| `mtix_claim` | Claim a node for an agent |
| `mtix_done` | Mark a node as done |
| `mtix_show` | Show full details of a node |
| `mtix_list` | List nodes with filtering and pagination |
| `mtix_blocked` | List blocked nodes with blocker details |
| `mtix_dep_show` | Show blocking dependencies for a node |
| `mtix_stats` | Get project statistics (node counts by status) |
| `mtix_search` | Search nodes with filters |

---

## TEST-DRIVEN DEVELOPMENT — NON-NEGOTIABLE

Exact TDD sequence (mandatory):

```
READ spec → WRITE test (red) → RUN test (fails) → WRITE code → RUN test (green) → REFACTOR → CHECK coverage → COMMIT
```

### Test Naming Pattern
```
Test{Function}_{Condition}_{ExpectedResult}
```

### mgit-Specific Examples
```go
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {}
func TestCreateCommit_WithDuplicateTaskID_ReturnsError(t *testing.T) {}
func TestSquashTask_AtomicFailure_RollsBackAllChanges(t *testing.T) {}
func TestSquashTask_EmptyTask_NoOp(t *testing.T) {}
func TestRollback_AppendOnly_CreatesRevertCommit(t *testing.T) {}
func TestRollback_Idempotent_SecondCallNoOp(t *testing.T) {}
func TestVerifyBranchMapping_WithIncorrectTask_ReturnsFalse(t *testing.T) {}
```

### Required Test Categories
1. **Happy path** — normal operation succeeds
2. **Error path** — invalid input rejected gracefully
3. **Boundary** — edge cases (empty commits, max size, zero tasks)
4. **Squash atomicity** — all-or-nothing semantics
5. **Rollback idempotence** — can rollback multiple times safely
6. **Append-only enforcement** — no history rewriting allowed
7. **go-git integration** — commits appear correctly in git log

### Table-Driven Tests (Mandatory)
```go
tests := []struct {
    name      string
    taskID    string
    commits   []*Commit
    wantErr   bool
    wantCount int
}{
    {
        name:      "single_commit",
        taskID:    "MTIX-1.2",
        commits:   []*Commit{testCommit1},
        wantErr:   false,
        wantCount: 1,
    },
    {
        name:      "duplicate_task_id",
        taskID:    "MTIX-1.2",
        commits:   []*Commit{testCommit1, testCommitDuplicateTask},
        wantErr:   true,
        wantCount: 0,
    },
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        // test body
    })
}
```

### Coverage Requirements Table

| Layer | Line Coverage | Branch Coverage |
|-------|---|---|
| Store (go-git + SQLite) | 95% | 90% |
| Service (Commit, Squash, Rollback) | 95% | 90% |
| Model / CLI / API / MCP | 90% | 85% |
| **Overall** | **90% min** | **85% min** |

Verify with: `go test ./... -coverprofile=cover.out && go tool cover -func=cover.out`

---

## ARCHITECTURE AND CODING RULES

### Layered Architecture

```
┌─────────────────────────────────────────┐
│ CLI Commands / REST Handlers / MCP Tools │
└──────────────┬──────────────────────────┘
               │
        ┌──────▼──────┐
        │ Service Layer │ ← ALL business logic
        │ (Commit,      │
        │  Squash,      │
        │  Rollback)    │
        └──────┬────────┘
               │
        ┌──────▼────────────────┐
        │ Store Interfaces      │ ← Data access contracts
        └──────┬────────────────┘
              / \
             /   \
    ┌───────▼─┐  ┌─────────────┐
    │ go-git  │  │ SQLite Index │
    │ Store   │  │ Store       │
    └─────────┘  └─────────────┘
```

### Dependency Rules
- Handlers **never** access Store directly (always via Service)
- Store layer = data access only (CRUD + transactions)
- Service layer = business logic (Commit, Squash, Rollback semantics)
- Model layer = types and errors (depends on nothing except stdlib)
- Dependency direction: **cmd/ → service/ → store/ → model/**

### General Principles
- **Dependency injection everywhere** — no global state, no singletons
- **Clock injection** — pass `func() time.Time` instead of calling `time.Now()` directly
- **Error handling** — wrap errors with `%w`, use sentinel errors from `model/errors.go`

### Sentinel Errors (in model/errors.go)
```go
var (
    ErrCommitNotFound     = errors.New("commit not found")
    ErrTaskNotFound       = errors.New("task not found")
    ErrBranchNotFound     = errors.New("branch not found")
    ErrSquashFailed       = errors.New("squash failed")
    ErrRollbackConflict   = errors.New("rollback conflict")
    ErrChainBroken        = errors.New("commit chain broken")
    ErrVerificationFailed = errors.New("verification failed")
    ErrAppendOnlyViolation = errors.New("append-only constraint violated")
    ErrBranchInUse         = errors.New("branch checked out in another worktree")
    ErrTaskAlreadyBound    = errors.New("task already bound to a worktree")
    ErrTaskMismatch        = errors.New("commit task ID does not match worktree binding")
    ErrWorktreeNotFound    = errors.New("worktree not found")
)
```

---

## SQL RULES

**These are not suggestions. These are laws.**

### Rule 1: Parameterized Queries Only
```go
// ✓ CORRECT
row := db.QueryRow("SELECT id FROM commits WHERE task_id = ?", taskID)

// ✗ FORBIDDEN
row := db.QueryRow(fmt.Sprintf("SELECT id FROM commits WHERE task_id = '%s'", taskID))
```

### Rule 2: PRAGMAs on Every Connection
```go
sqliteConn.Exec("PRAGMA foreign_keys = ON")
sqliteConn.Exec("PRAGMA journal_mode = WAL")
sqliteConn.Exec("PRAGMA busy_timeout = 5000")
sqliteConn.Exec("PRAGMA synchronous = FULL")
```

### Rule 3: All Writes in Transactions
```go
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
    tx, err := s.writeDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    if err != nil { return fmt.Errorf("begin tx: %w", err) }
    defer tx.Rollback()
    if err := fn(tx); err != nil { return err }
    return tx.Commit().Err
}
```

### Rule 4: Separate Read/Write Connection Pools
```go
s.readDB = sqliteDB  // Allow many readers
s.writeDB = sqliteDB // Single writer
s.writeDB.SetMaxOpenConns(1) // Enforce mutual exclusion
```

### Rule 5: task_commits Table is APPEND-ONLY
```sql
CREATE TABLE task_commits (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
    -- NO UPDATE, NO DELETE triggers/checks
);
```

Never execute `UPDATE` or `DELETE` on `task_commits`. Ever. Audit requires complete history.

---

## go-git RULES

### Rule 1: NEVER Shell Out to git
```go
// ✗ FORBIDDEN
exec.Command("git", "commit", "-m", msg).Run()

// ✓ CORRECT
worktree.Commit(msg, &git.CommitOptions{Author: &object.Signature{}})
```

### Rule 2: Use Plumbing API for Determinism
```go
// Plumbing (lower-level, deterministic)
ref, err := repo.Reference(plumbing.HEAD, false)

// NOT porcelain (higher-level, less predictable)
// (avoid Worktree methods except where necessary)
```

### Rule 3: Wrap go-git Types
```go
// ✗ FORBIDDEN
type Commit struct {
    *object.Commit  // Direct exposure
}

// ✓ CORRECT
type Commit struct {
    ID       string    // Our own fields
    TaskID   string
    Hash     string
    Message  string
    Author   string
    Timestamp time.Time
}
// Helper to convert from go-git object.Commit
func newCommitFromGit(oc *object.Commit, taskID string) *Commit { ... }
```

### Rule 4: All Commit Metadata in git Commit Objects
- Task ID goes in commit message (structured format: `TASK_ID:...`)
- Author signature set deterministically
- Timestamp embedded in git object

### Rule 5: Dual-Hash Model (ADR-002)
- **Git object IDs** use SHA-1 (go-git native, git protocol requirement — unavoidable)
- **mgit content_hash** uses SHA-256 (mgit's own integrity guarantee for audit/compliance)
- Store BOTH hashes in SQLite task_commits index
- Verify integrity: recompute SHA-1 from git object AND SHA-256 from mgit metadata
- ContentHash (SHA-256) is the authoritative hash for regulatory compliance
- See ADR-002-DUAL-HASH-MODEL.md for full rationale

---

## APPROVED PACKAGES

| Package | Purpose | Min Version | Rationale |
|---------|---------|---------|-----------|
| `go-git/go-git/v5` | Pure Go git engine | ≥ 5.13.0 | No shell execution, deterministic, audit-trail compatible |
| `modernc.org/sqlite` | Pure Go SQLite | ≥ 1.35.0 | CGO-free, embedded, ACID transactions |
| `spf13/cobra` | CLI framework | ≥ 1.8.1 | Industry standard, well-tested |
| `labstack/echo/v4` | HTTP framework | ≥ 4.13.4 | Lightweight, middleware support for REST API |
| `oklog/ulid/v2` | ULID generation | ≥ 2.1.0 | Sortable IDs, cryptographically random |
| `stretchr/testify` | Test assertions | ≥ 1.10.0 | Clean assert syntax |
| `golang.org/x/sync` | errgroup | Latest | Concurrent error handling |
| `log/slog` (stdlib) | Structured logging | Go 1.22+ | Built-in, no dependencies |

### Explicitly Rejected
- ✗ `mattn/go-sqlite3` (CGO, C dependency, supply chain risk)
- ✗ `GORM` (ORM overhead, hides SQL, reduces auditability)
- ✗ `sqlx` (query builder, still too magic)
- ✗ `logrus` (archived, prefer slog)
- ✗ `pkg/errors` (superseded by stdlib wrapping)
- ✗ External git binary (determinism killer)
- ✗ `go-git/go-git/v6` (not yet approved — see ADR-004; planned for future evaluation once mature)

---

## WORKTREE CONVENTIONS (FR-16)

mgit supports linked worktrees for multi-agent parallel development. The CLI mirrors standard git worktree semantics so agents trained on git can use them without friction.

### Key Rules

1. **Every worktree is bound to exactly one task.** `mgit worktree add` requires `--task <ID>`. Commits from within a worktree auto-inherit the bound task ID.
2. **No branch sharing.** Two worktrees cannot have the same branch checked out.
3. **No task sharing.** Two worktrees cannot be bound to the same task ID.
4. **Pluggable backend.** The `WorktreeManager` interface (FR-16.10) abstracts worktree mechanics. v1 is mgit-managed; future versions may delegate to go-git v6 native worktrees (ADR-004).

### WorktreeManager Interface
```go
type WorktreeManager interface {
    Add(ctx context.Context, opts WorktreeAddOptions) (*WorktreeInfo, error)
    List(ctx context.Context) ([]WorktreeInfo, error)
    Remove(ctx context.Context, path string, force bool) error
    Prune(ctx context.Context, dryRun bool) ([]string, error)
    Resolve(ctx context.Context, path string) (*WorktreeInfo, error)
}
```

### Where Worktree Code Lives
- `internal/model/worktree.go` — WorktreeInfo, WorktreeAddOptions, WorktreeManager interface
- `internal/store/index/worktrees.go` — SQLite registry CRUD
- `internal/worktree/manager.go` — v1 WorktreeManager implementation
- `internal/service/worktree_service.go` — Lifecycle orchestration, task binding, hooks
- `cmd/mgit/worktree.go` — CLI subcommands

---

## NAMING AND FORMATTING

### Export/Unexport Casing
```go
// Exported (public API)
type Commit struct { ... }
func (c *Commit) SHA() string { ... }

// Unexported (internal)
func (c *commit) validate() error { ... }
type commitIndex struct { ... }
```

### Error Naming
```go
var ErrCommitNotFound = errors.New("commit not found")  // ✓
var CommitNotFoundErr = errors.New("commit not found")  // ✗
```

### Test Naming
```go
func TestCommitService_CreateCommit_ValidInput_Success(t *testing.T) { ... }
func TestStore_QueryByTaskID_NotFound_Error(t *testing.T) { ... }
```

### Import Organization
```go
import (
    "context"
    "crypto/sha256"
    "fmt"
    // blank line
    "github.com/go-git/go-git/v5"
    "github.com/labstack/echo/v4"
    // blank line
    "mgit/internal/model"
    "mgit/internal/service"
    "mgit/internal/store"
)
```

### JSON Tags (snake_case)
```go
type CommitResponse struct {
    TaskID    string `json:"task_id"`
    CommitHash string `json:"commit_hash"`
    CreatedAt  string `json:"created_at,omitempty"`
}
```

### Timestamps (ISO-8601 UTC)
```go
timestamp := time.Now().UTC().Format(time.RFC3339)  // ✓
timestamp := time.Now().String()                     // ✗
```

---

## FUNCTION AND FILE LIMITS

| Metric | Limit | Rationale |
|--------|-------|-----------|
| Function body | 50 lines | Readability, testability, review cost |
| Cyclomatic complexity | ≤ 15 | Branch coverage feasibility |
| Cognitive complexity | ≤ 20 | Mental model fit |
| Parameters per function | ≤ 5 | Consider struct or options pattern if more needed |
| File length | ≤ 500 lines | Cohesion, navigation |

Violations require doc comments explaining why and approval from senior reviewer.

---

## DOCUMENTATION REQUIREMENTS

### Godoc on Every Exported Symbol
```go
// Commit represents a task-tagged micro-commit.
// Task ID is stored in the commit message and indexed in SQLite
// for O(1) lookup by task. All commits are append-only.
// Refs: FR-1.1, FR-1.2, NFR-1.1
type Commit struct {
    ID    string    // Unique commit ID (SHA-1 hash)
    TaskID string   // Task identifier (e.g., "MTIX-2.1")
}

// CreateCommit atomically creates a commit and indexes it by task.
// If task ID already exists, returns ErrDuplicateTask.
// Refs: FR-1.3, NFR-1.2
func (s *CommitService) CreateCommit(ctx context.Context, taskID, msg string) (*Commit, error) {
```

### FR/NFR Traceability in Godoc
- Every function doing substantive work must reference requirements
- Format: `Refs: FR-1.1, FR-1.2, NFR-2.3`
- Allows reviewer to trace code → spec

### Inline Comments for All SQL Queries
```go
// Fetch all commits for a given task (single-task lookup, O(1) via index)
const queryByTask = `SELECT id, commit_hash FROM task_commits WHERE task_id = ?`
rows, err := s.readDB.QueryContext(ctx, queryByTask, taskID)
```

---

## COMMIT MESSAGE FORMAT

```
{type}({scope}): {description}

{body}

Refs: MGIT-{task-id}
```

### Types
- `feat` — new feature (e.g., Squash service)
- `fix` — bug fix (e.g., Fix rollback atomicity)
- `test` — test coverage (e.g., Add table-driven tests for Store)
- `refactor` — code reorganization (no behavior change)
- `docs` — documentation or comments
- `perf` — performance improvement
- `chore` — tooling, CI, dependencies

### Scopes
- `store` — SQLite or go-git store layer
- `service` — Commit, Squash, or Rollback service
- `cli` — Command-line interface
- `api` — REST handlers
- `mcp` — MCP tool integration
- `model` — Types and errors
- `docs` — README, CLAUDE.md, inline docs
- `git` — go-git wrapper

### Example
```
feat(service): implement squash with atomic rollback

When squashing multiple micro-commits for a task, if any
insert fails, roll back all changes atomically via SQLite
transaction. Prevents partial squashes from polluting git repo.

Validated with TestSquashTask_AtomicFailure_RollsBackAllChanges.

Refs: MGIT-3.1
```

---

## STATIC ANALYSIS

### Required Tools
```bash
golangci-lint run ./...
go vet ./...
go test ./... -race
govulncheck ./...
```

### Zero-Tolerance Policy
- **Zero linter warnings** — no suppressions without justified doc comment
- **Zero test failures** — all tests must pass before commit
- **Zero race detector warnings** — -race flag on all test runs
- **Zero known vulnerabilities** — govulncheck passes

Example of justified suppression:
```go
//nolint:gocyclo // OK: branch conditions are all independent safety checks
func validateCommit(c *Commit) error {
```

---

## SECURITY MINDSET

### Treat All Input as Hostile
- CLI flags can be malformed, oversized, or injection attacks
- REST payloads can exceed size limits, contain control characters
- MCP tool inputs are untrusted even from "trusted" agents

### SQL Injection is Enemy #1
- Every parameter must use `?` placeholders
- String concatenation in SQL is instant code review rejection
- Audit: search codebase for `Sprintf` near SQL — burn with fire

### Append-Only Enforcement
- No UPDATE or DELETE on audit tables
- Rollbacks create new "revert" commits, not erasure
- Verify: read task_commits table size, should only grow

### Localhost Binding by Default
```go
echo.Start(":8080") → change to "127.0.0.1:8080"
```
- If multi-host access needed, require explicit config + docs

### Content Addressing Prevents Tampering
- Every commit has SHA-1 hash (go-git native)
- Verify: recompute hash from git object
- If hashes diverge, commit was tampered with

### Dependency Supply Chain Control
- Only approved packages from APPROVED-PACKAGES.md
- Pin versions in go.mod
- `go mod tidy` is your friend (detects unused deps)
- Annual audit of transitive dependencies

---

## THE 12 COMMANDMENTS

1. **No production code without failing test first**
   - Write test, see it fail (red), write code, see it pass (green)
   - Reverse order (code then test) is rejected outright

2. **No string concatenation in SQL**
   - ALWAYS use `?` placeholders and pass args separately
   - `fmt.Sprintf("... WHERE id = '%s'", id)` → instant rejection

3. **No unapproved packages**
   - Check APPROVED-PACKAGES.md before `go get`
   - If package not listed, propose it in writing (doc + rationale), get approval

4. **No swallowed errors**
   - Every error return must be checked: `if err != nil { return ... }`
   - `_ = someFunc()` is forbidden without doc comment explaining why

5. **No global state / singletons / init()**
   - Dependency inject everything: *Store, *Service, clock func
   - init() functions are red flags (hard to test, implicit side effects)

6. **No time.Now() — inject clock**
   - Accept `clock func() time.Time` as parameter
   - Allows time-machine testing (frozen time, leap seconds, etc.)

7. **No bypassing service layer from handlers**
   - CLI commands call Service, Service calls Store
   - Never `cli.go` → directly to `store.go`

8. **No files > 500 lines or functions > 50 lines**
   - If code is too long to reason about, refactor
   - Long functions hide bugs, complicate testing, violate SRP

9. **No commits with linter warnings or failing tests**
   - Run before commit: `go test ./...`, `golangci-lint run`
   - Pre-commit hook recommended

10. **No skipping pre-work protocol**
    - Read task, read requirements, read governing docs, read existing code
    - Skipping leads to rework, delays, and lost trust

11. **No marking tasks "done" with stubs or skeleton code**

    A task is **only** done when both of the following are true:
    - **Code complete**: every acceptance criterion in the task prompt is implemented in real, working code (not stubs, not placeholders, not fake success returns)
    - **Tests complete**: every test listed in the task's "REQUIRED TESTS" section exists, passes, and verifies the corresponding behavior

    If either condition is not met, the task is **NOT done**, regardless of how much surrounding work is finished. Use `mtix_dep_add` and the blocked status if you cannot complete it now.

    A task is NOT done if any of these are true:
    - The implementation prints "not implemented", returns fake success, or contains placeholder comments instead of real logic
    - A function returns `nil` without doing real work, or a handler returns `{"status": "completed"}` without performing the actual operation
    - The code contains `// TODO: implement`, `// integration pending`, or similar
    - A required CLI flag from the task prompt is missing
    - A required test from the task prompt does not exist in the codebase
    - The acceptance criteria mention behavior that the code does not exhibit

    Stub code that compiles but does nothing is **worse** than code that returns an error — it is an active deception that hides incomplete work.

    **Mandatory verification before calling `mtix_done`:**

    1. Re-read the task prompt and acceptance criteria one more time
    2. For every flag, function, or behavior mentioned, grep the codebase to confirm it exists
    3. For every test name in REQUIRED TESTS, run `grep -rn 'func TestName' --include='*.go'` and confirm it exists
    4. Run the required tests individually and confirm they pass: `go test -run 'TestName' ./path/...`
    5. Run the anti-stub grep: `grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"\|TODO: implement' --include='*.go' --exclude='*_test.go'`
    6. If any check fails, the task is not done. Mark it blocked or leave it open.

    **Marking a stub task done is a process violation that will be caught by audit and require rework.** Honest "blocked" status is always better than dishonest "done" status.

12. **No coverage-chasing test files**
    - Test files MUST be named after the functionality they test, not after why they were written
    - ✓ `store_operations_test.go`, `service_edge_cases_test.go`, `mcp_tool_integration_test.go`
    - ✗ `coverage_boost_test.go`, `coverage_boost2_test.go`, `gap_filler_test.go`
    - Tests exist to verify behavior, not to satisfy metrics

---

## VERIFICATION CHECKLIST

Run before marking task "done":

```bash
# 1. Unit tests (single-threaded)
go test ./... -count=1

# 2. Race detector (concurrent safety)
go test ./... -race -count=1

# 3. Coverage (must be ≥ 90% line, ≥ 85% branch)
go test ./... -coverprofile=cover.out -count=1
go tool cover -func=cover.out | tail -1

# 4. Linter (zero warnings)
golangci-lint run

# 5. Vulnerability check
govulncheck ./...

# 6. Build (must produce binary)
go build -o mgit ./cmd/mgit/

# 7. Smoke test (manual or script)
./mgit --help
./mgit commit --help
./mgit squash --help
./mgit rollback --help

# 8. Code review readiness
# - Godoc on every export
# - Comments on all SQL
# - FR/NFR traceability
# - No files > 500 lines
# - No functions > 50 lines
# - Error handling on every error return

# 9. Commit message
# - Type, scope, description correct
# - References MGIT-x.y task ID
# - No linter suppressions without justification

# 10. Anti-stub verification
# - No "not implemented" / "not yet implemented" / "integration pending" in changed files
grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"' \
  --include='*.go' --exclude='*_test.go'
# Must return zero matches in your changed files

# 11. Task status update (via mtix)
# - Call mtix_done with your task ID
# - Verify with mtix_show that status is "done"
```

---

## REMEMBER

You are not writing code. You are writing **trust**.

Every commit you submit will be read by:
- Hospital compliance auditors (Is this safe?)
- Airline accident investigators (What was the LLM thinking?)
- NASA engineers (Can we fly this?)
- DoD program managers (Does it meet standards?)
- Surgeons (Will this kill someone?)

Your code is a legal and safety document. It must be correct not just in spirit, but in fact — provably correct, testably correct, reviewably correct.

The bug you skip today might cost lives tomorrow.

Write with that in mind.

---

**Last updated:** 2026-03-09
**Applies to:** mgit v1.0 development
**Governed by:** DO-178C, IEC 62304, NASA-STD-8739.8, OWASP ASVS L2
