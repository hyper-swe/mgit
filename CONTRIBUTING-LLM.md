# CONTRIBUTING-LLM.md
## Contributing to mgit as an LLM Coding Agent

### 1. Introduction

You are an LLM coding agent contributing to **mgit**, a safety-critical micro version control system designed for use in high-assurance environments including NASA, aviation, healthcare, and Department of Defense systems.

mgit is not a learning project. Every line of code you write may eventually run in life-critical or mission-critical systems. Your contributions must meet the same rigor, testing standards, and architectural discipline as human-authored safety-critical code in DO-178C Level A or IEC 61508 SIL 4 environments.

Before proceeding with any work on mgit:
1. Read CLAUDE.md in full. This document establishes your operational boundaries and safety constraints.
2. Read this document in full. It defines the technical requirements for your contributions.
3. Read REQUIREMENTS.md to understand the functional and non-functional requirements you must satisfy.
4. Familiarize yourself with TDD-WORKFLOW.md, CODING-STYLE.md, and QUALITY-STANDARDS.md.

**Non-negotiable premise**: Correct, well-tested, well-documented code is always preferred over fast code. A feature that takes twice as long but is twice as reliable is the right trade.

---

### 2. Operating Principles

These principles guide every decision you make while contributing to mgit.

**Correctness over Speed**

A correct implementation that takes longer is always preferred over a fast but flawed one. If a task is estimated at 2 hours but your careful analysis reveals it will take 4 hours to do correctly, report that. Time pressure is never an excuse for cutting corners on safety-critical software.

- Every function must be verifiable. It must be possible to prove it works correctly.
- Every edge case must be identified and handled explicitly.
- Defensive programming is mandatory: assume inputs are wrong until validated.
- If you are uncertain whether your implementation is correct, do not commit it.

**Explicit over Implicit**

Never assume behavior. Explicit is always better than implicit. This prevents bugs caused by misunderstanding.

- Do not guess at requirements. Verify every requirement against REQUIREMENTS.md.
- Do not assume error handling. Explicitly check every error return value.
- Do not use magic numbers. Use named constants with comments explaining their origin.
- Do not infer behavior from examples. Read the specification document.
- If a specification is ambiguous, document the ambiguity, commit the documentation, and set the task to "blocked" pending clarification.

**Test-First Development (Mandatory)**

Test-Driven Development (TDD) is not optional for mgit contributions. Every single feature, fix, and refactoring follows this sequence:

1. Write the test first. Verify it fails (red).
2. Write the minimal implementation to pass the test (green).
3. Refactor and optimize while keeping tests green.

Tests serve multiple purposes: they define behavior, prevent regressions, provide documentation, and catch mutations early.

See TDD-WORKFLOW.md for detailed guidance on test structure, table-driven tests, and integration test patterns.

**Traceability**

Every public function must trace back to a requirement. Every function that implements a Functional Requirement (FR) or Non-Functional Requirement (NFR) must have a comment in its Godoc linking to that requirement by ID.

Example:
```go
// SquashCommits merges N commits into one, maintaining immutable history.
// FR-012: Squash commits preserve full history via revert chain.
// NFR-007: Squash atomicity — all-or-nothing operation.
func (s *CommitService) SquashCommits(ctx context.Context, ...) error {
```

This ensures impact analysis is always possible: if a requirement changes, we know exactly which functions are affected.

**Append-Only Thinking**

In mgit, history is never rewritten. Data is never deleted. This mindset must permeate all your design decisions:

- Do not mutate existing records.
- Use timestamps and versioning to represent state changes.
- When reverting changes, append a revert commit (never delete the original).
- When rolling back work, create a new commit that undoes the original (never rewrite history).
- SQLite indexes are append-only. The task_commits table only grows; it never shrinks.

This pattern ensures auditability, detectability of tampering, and recovery from corruption.

---

### 3. Task Workflow (via mtix MCP)

All work on mgit is tracked by **mtix** (micro-tix), the companion AI-native micro issue manager. You interact with tasks via mtix MCP tools — NOT by editing JSON files directly.

**Picking Up a Task**

1. Call `mtix_ready` to find tasks that are unblocked and unclaimed.
2. Call `mtix_context` with the chosen task ID to get the full ancestor chain:
   - **Story prompt** — project-wide context (architecture, standards, safety requirements)
   - **Epic prompt** — feature-area context (specific subsystem, interfaces, patterns)
   - **Issue prompt** — implementation specifics (exact functions, tests, acceptance criteria)
   - The assembled prompt gives you everything needed to implement the task correctly.
3. Call `mtix_claim` with your agent ID to claim the task (prevents other agents from picking it up).

**Status Transitions (via mtix MCP)**

```
open → in_progress (mtix_claim — you claim the task)
in_progress → done (mtix_done — implementation complete, all tests pass)
in_progress → blocked (automatic — when unresolved blocking dependencies exist via mtix_dep_add)
```

Blocked status is auto-managed by mtix. When a node has unresolved blocking dependencies, it transitions to blocked automatically. Use `mtix_dep_add` to declare a dependency and `mtix_dep_show` to inspect blockers.

**Key mtix MCP Tools**

| Tool | Purpose |
|------|---------|
| `mtix_ready` | List nodes ready for work (unblocked, unassigned) |
| `mtix_context` | Assemble context chain for a node (ancestors, siblings, blocking deps, prompt) |
| `mtix_claim` | Claim a node for an agent |
| `mtix_done` | Mark a node as done |
| `mtix_show` | Show full details of a node |
| `mtix_dep_show` | Show blocking dependencies for a node |

**Commit Message Format**

Every commit related to a task must reference the task ID:

```
{type}({scope}): {description}

Refs: MGIT-{id}

Details explaining the change (optional but recommended for safety-critical work).
```

Examples:
```
feat(store): implement task_commits table with append-only semantics

Refs: MGIT-007

Added INSERT-only triggers on task_commits to prevent accidental
deletes or updates. Includes comprehensive integration tests covering
edge cases around concurrent inserts.

test(store): add tests for squash operation atomicity

Refs: MGIT-012

Introduced TestSquashAtomicity which verifies that if any step of a
squash operation fails, the database rolls back completely and no
partial state is left behind.
```

**One Task = One Logical Commit**

Every task results in exactly one logical commit that includes:
- All test code (unit tests, integration tests)
- All production code
- Updated Godoc with FR/NFR references
- Updated CHANGELOG.md

Test code and production code are committed together. Never commit production code without tests.

---

### 4. Code Quality Requirements

mgit is held to strict code quality standards. These are not suggestions; they are requirements.

**Coverage Targets**

- `store/` (SQLite interface layer): **95% line coverage minimum**
- `service/` (business logic layer): **95% line coverage minimum**
- `model/` (domain types): **90% line coverage minimum**
- `cli/` (command-line interface): **90% line coverage minimum**
- `api/` (HTTP API, if implemented): **90% line coverage minimum**
- `mcp/` (MCP interface, if implemented): **90% line coverage minimum**

Measure coverage with `go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out`. No package falls below its target without explicit exemption (documented in code comments with justification).

**Static Analysis (Zero Warnings)**

Before submitting a pull request, run:

```bash
golangci-lint run ./...
go vet ./...
go test ./... -race
govulncheck ./...
```

All output must be clean. Zero warnings. No exceptions.

- `golangci-lint` catches style, correctness, and complexity issues.
- `go vet` catches suspicious patterns.
- `-race` flag detects data races (memory safety).
- `govulncheck` ensures no known vulnerabilities in dependencies.

**Function Size Limits**

- **Function body**: Maximum 50 lines (not including comments or blank lines).
- **Cyclomatic complexity**: Maximum 15 (measured by `gocyclo`).
- **Parameters**: Maximum 5 parameters. Use structs for related parameters.

Functions that exceed these limits are hard to test, hard to understand, and error-prone. Refactor into smaller functions.

**File Size Limits**

- **Per-file limit**: Maximum 500 lines of code (not including package comment or blank lines).

Files that exceed 500 lines are candidates for splitting into multiple files by responsibility.

**Test File Naming**

Test files MUST be named after the functionality they test, not after why they were written:

```
# ✅ CORRECT — describes what is tested
store_operations_test.go
cli_edge_cases_test.go
service_edge_cases_test.go
mcp_tool_integration_test.go

# ❌ FORBIDDEN — describes why the file exists
coverage_boost_test.go
coverage_boost2_test.go
gap_filler_test.go
```

**Godoc Documentation**

Every exported symbol (function, type, constant, variable) must have Godoc:

```go
// SquashCommits merges N commits into a single commit, preserving full history.
// The operation is atomic: if any step fails, the entire operation rolls back.
// FR-012: Squash commits preserve full history via revert chain.
// NFR-007: Squash atomicity.
func (s *CommitService) SquashCommits(ctx context.Context, taskID string, commitIDs []string) error {
```

Godoc must:
- Explain what the function does.
- Explain preconditions (what must be true before calling).
- Explain postconditions (what is true after calling).
- Reference the FR/NFR numbers it satisfies.
- Mention any error conditions and what errors are returned.

---

### 5. Architecture Rules

mgit follows a strict layered architecture. Breaking these rules introduces technical debt, security issues, and testability problems.

**Layered Architecture**

```
cmd/
├─ mgit/              # CLI entry point
└─ (no business logic here)

cli/                  # Command handlers (optional, if separating CLI from API)
├─ status.go
└─ commit.go

service/              # Business logic layer
├─ commit_service.go
├─ squash_service.go
└─ interfaces.go      # All Store, Repository interfaces defined here

store/                # SQLite + go-git access layer
├─ task_commits.go
├─ commit_index.go
└─ impl.go            # Implementations of service/ interfaces

model/                # Domain types (pure Go, no I/O)
├─ task.go
├─ commit.go
└─ errors.go

```

**Dependency Direction: Downward Only**

- `cmd/` imports `cli/` and `service/`
- `cli/` imports `service/` and `model/`
- `service/` imports `model/`
- `store/` imports `model/` (and go-git)
- `model/` imports nothing (except stdlib and go-git types that are wrapped)

**This is non-negotiable**: Never import upward. If `store/` needs to import from `service/`, you have a circular dependency. Stop. Refactor.

**Interfaces Defined in service/, Implemented in store/**

All I/O interfaces are defined in `service/` (abstraction layer). They are implemented in `store/` (concrete layer).

Example:
```go
// service/interfaces.go
type TaskCommitStore interface {
    GetCommits(ctx context.Context, taskID string) ([]model.Commit, error)
    InsertCommit(ctx context.Context, taskID string, commit model.Commit) error
}

// store/task_commits.go
type TaskCommitStoreImpl struct {
    db *sql.DB
}

func (t *TaskCommitStoreImpl) GetCommits(ctx context.Context, taskID string) ([]model.Commit, error) {
    // Implementation
}
```

**model/ is Pure Types**

`model/` contains:
- Type definitions (struct, interface)
- Errors (var ErrCommitNotFound)
- Constants

`model/` does NOT contain:
- Database queries
- File I/O
- Network calls
- go-git operations (that's store/ responsibility)

**No Circular Imports**

Use `go mod graph` to verify. Circular imports are a sign of architectural confusion.

**Dependency Injection via Constructors**

Do not use global state or singletons. Inject dependencies via constructors:

```go
type CommitService struct {
    store TaskCommitStore
    repo  Repository
    clock func() time.Time
}

func NewCommitService(store TaskCommitStore, repo Repository, clock func() time.Time) *CommitService {
    return &CommitService{store, repo, clock}
}
```

This makes testing trivial (pass mock implementations) and the dependency graph explicit.

---

### 6. go-git Specific Rules

mgit uses `go-git v5` (github.com/go-git/go-git/v5) as its only Git implementation. Absolutely no `exec.Command("git", ...)`.

**NEVER Use exec.Command("git", ...)**

✗ FORBIDDEN:
```go
cmd := exec.Command("git", "commit", "-m", message)
err := cmd.Run()
```

✓ CORRECT:
```go
tree, _ := r.Filesystem.Index.BuildTree(stager)
commit := object.NewCommit(object.CommitObject{
    TreeHash: tree,
    // ...
})
```

Why? exec.Command is:
- Non-deterministic (depends on Git installation, version, environment)
- Unauditable (we cannot inspect what happened inside)
- Hard to test (you cannot mock it)
- Unsafe (shell injection risks if args are user-controlled)

**Prefer Plumbing API over Porcelain**

go-git has two APIs:
- **Porcelain**: high-level, user-friendly (Clone, Fetch, Pull)
- **Plumbing**: low-level, deterministic, auditable (WriteObject, UpdateRef)

For safety-critical work, prefer plumbing:

✗ Less ideal:
```go
commit, _ := r.CommitObject(hash) // Porcelain
```

✓ Preferred:
```go
obj, _ := r.Storer.EncodedObject(plumbing.CommitObject, hash)
dec := object.NewDecoder(obj)
commit := &object.Commit{}
dec.Decode(commit) // Plumbing with explicit steps
```

Plumbing is more verbose but more auditable and predictable.

**Wrap go-git Types — Never Expose Directly**

Never expose `*git.Repository`, `*object.Commit`, or other go-git types to callers. Wrap them in domain types:

✗ FORBIDDEN:
```go
func (s *CommitService) GetCommit(id string) (*object.Commit, error) {
    // Exposes go-git internals
}
```

✓ CORRECT:
```go
func (s *CommitService) GetCommit(ctx context.Context, id string) (model.Commit, error) {
    obj, _ := s.repo.GetCommit(hash)
    return model.Commit{
        Hash: obj.Hash.String(),
        Message: obj.Message,
        Author: obj.Author.Name,
        // ... mapped to domain type
    }, nil
}
```

This allows you to:
- Change the underlying Git implementation without breaking callers.
- Ensure type safety (domain types are validated).
- Add mgit-specific metadata (task ID, agent ID, session ID).

**Error Conversion: go-git → mgit Domain Errors**

go-git returns `plumbing.ErrObjectNotFound`, `plumbing.ErrReferenceNotFound`, etc. Convert these to mgit domain errors:

✗ FORBIDDEN (leaking go-git errors):
```go
obj, err := r.Storer.EncodedObject(plumbing.CommitObject, hash)
return obj, err // Returns go-git error to caller
```

✓ CORRECT (wrapped domain error):
```go
obj, err := r.Storer.EncodedObject(plumbing.CommitObject, hash)
if err == plumbing.ErrObjectNotFound {
    return nil, fmt.Errorf("commit not found: %w", model.ErrCommitNotFound)
}
if err != nil {
    return nil, fmt.Errorf("get commit: %w", err)
}
return obj, nil
```

**Commit Creation Order (CRITICAL)**

When creating a commit, follow this exact order:

1. Build the tree object from staged content.
2. Create the commit object with metadata (message, author, timestamps, parent references, task ID, agent ID, session ID).
3. Write the commit object to the repository storer.
4. Update the reference (branch) to point to the new commit.
5. Update the SQLite index with the mapping (task ID → commit hash).

If any step fails, roll back all previous steps. This is critical for consistency.

```go
tree, _ := buildTree(...)
commit := object.NewCommit(object.CommitObject{
    TreeHash: tree,
    Message: msg,
    Author: ...,
    // Embed mgit metadata
})
err := r.Storer.SetEncodedObject(commit)
if err != nil { /* rollback */ }

ref := plumbing.NewHashReference(plumbing.HEAD, commit.Hash)
err = r.Storer.SetReference(ref)
if err != nil { /* rollback */ }

err = s.store.InsertCommit(ctx, taskID, modelCommit)
if err != nil { /* rollback */ }
```

**Deterministic Commit IDs**

Commit IDs (SHA-256 hashes) are derived from commit content:
- Tree hash
- Parent commit hash
- Author (name, email, timestamp)
- Committer (name, email, timestamp)
- Message

This means the same logical commit, created identically, produces the same hash. This is a feature (determinism), not a bug.

**Metadata Embedding**

All mgit-specific metadata (task ID, agent ID, session ID) is embedded in the commit message or as Go-git extended headers. Never store it separately in a way that can diverge from the commit itself.

---

### 7. SQLite Index Rules

The SQLite database (task_commits index) is a critical component. It is not a cache; it is the source of truth for task↔commit relationships.

**APPEND-ONLY: task_commits Table**

The `task_commits` table has exactly one allowed operation: INSERT.

✗ FORBIDDEN:
```go
DELETE FROM task_commits WHERE commit_hash = ?
UPDATE task_commits SET status = ? WHERE id = ?
TRUNCATE TABLE task_commits
```

✓ CORRECT:
```go
INSERT INTO task_commits (task_id, commit_hash, created_at, agent_id) VALUES (?, ?, ?, ?)
```

Why append-only?
- Every row represents a historical fact: "This commit belongs to this task."
- Deleting a row erases that fact, making it unauditable.
- Updating a row rewrites history, which violates the immutability principle.

To implement "logical deletion", use a `deleted_at` timestamp:
```go
-- Mark as logically deleted, never actually delete
UPDATE task_commits SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL
```

But this is only done when absolutely necessary and with great care.

**Parameterized Queries ONLY**

Every SQL query must use parameter binding. Zero exceptions.

✗ FORBIDDEN (SQL injection):
```go
query := fmt.Sprintf("SELECT * FROM task_commits WHERE task_id = '%s'", taskID)
row := db.QueryRow(query)
```

✓ CORRECT (parameterized):
```go
query := "SELECT * FROM task_commits WHERE task_id = ?"
row := db.QueryRow(query, taskID)
```

Even if taskID is "from a trusted source," parameterize it. This is a hill to die on.

**PRAGMAs on Every Connection**

Configure SQLite pragmatically for safety and concurrency:

```go
func initDB(path string) (*sql.DB, error) {
    db, _ := sql.Open("sqlite", path)

    // Foreign key constraints
    db.Exec("PRAGMA foreign_keys = ON")

    // Write-ahead logging for concurrency
    db.Exec("PRAGMA journal_mode = WAL")

    // Timeout for busy database (5 seconds)
    db.Exec("PRAGMA busy_timeout = 5000")

    return db, nil
}
```

- `foreign_keys = ON`: Enforce referential integrity.
- `journal_mode = WAL`: Write-ahead logging allows concurrent readers.
- `busy_timeout = 5000`: Wait up to 5 seconds if database is locked (prevents errors on contention).

**Separate Read and Write Connection Pools**

SQLite allows only one writer at a time. To prevent write starvation, use separate pools:

```go
// Write pool: strictly limited to 1 connection
writeDB.SetMaxOpenConns(1)
writeDB.SetMaxIdleConns(1)

// Read pool: can be larger
readDB.SetMaxOpenConns(10)
readDB.SetMaxIdleConns(5)
```

**All Writes in Transactions**

Use a `withTx` helper for transactional writes:

```go
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
    tx, _ := s.writeDB.BeginTx(ctx, nil)
    err := fn(tx)
    if err != nil {
        tx.Rollback()
        return err
    }
    return tx.Commit().Err
}

// Usage
err := s.withTx(ctx, func(tx *sql.Tx) error {
    _, err := tx.ExecContext(ctx, "INSERT INTO task_commits ...", args...)
    return err
})
```

All writes (INSERT, UPDATE, DELETE) happen inside `withTx`. This ensures atomicity.

**Bidirectional Queries**

The index must support:
- **Forward**: Given a task ID, find all commits for that task.
- **Reverse**: Given a commit hash, find the task(s) it belongs to.

Both queries must be efficient (indexed).

```sql
CREATE TABLE task_commits (
    id INTEGER PRIMARY KEY,
    task_id TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    agent_id TEXT,
    UNIQUE(task_id, commit_hash)
);

CREATE INDEX idx_task_commits_task_id ON task_commits(task_id);
CREATE INDEX idx_task_commits_commit_hash ON task_commits(commit_hash);
```

---

### 8. Safety-Critical Patterns

These patterns are mandatory. They exist to prevent data corruption and ensure auditability.

**Squash Atomicity**

Squashing N commits into one is an all-or-nothing operation. If any of these steps fails, the entire operation rolls back and the database is left untouched:

1. Validate all source commits exist and form a valid chain.
2. Build a new tree from the final state.
3. Create a squashed commit object.
4. Write the commit object.
5. Update the reference (branch).
6. Update the task_commits index.

If step 4 fails, steps 1-3 have no effect. If step 5 fails, the database rolls back. No partial squash.

```go
err := s.store.withTx(ctx, func(tx *sql.Tx) error {
    // Step 1: Validate
    for _, id := range commitIDs {
        _, err := s.getCommit(ctx, id)
        if err != nil { return err }
    }

    // Step 2-4: Build and write
    tree, _ := buildTree(...)
    commit := createCommit(tree, ...)
    r.Storer.SetEncodedObject(commit)

    // Step 5-6: Update refs and index
    r.updateRef(commit.Hash)
    tx.ExecContext(ctx, "INSERT INTO task_commits ...", ...)

    return nil
})
```

**Rollback as Append (Revert Commit)**

When rolling back work, never delete or rewrite commits. Instead, create a new **revert commit** that undoes the original:

✗ FORBIDDEN (history rewriting):
```go
// Delete the bad commit
DELETE FROM task_commits WHERE commit_hash = ?
r.updateRef(parent_commit_hash) // Move HEAD back
```

✓ CORRECT (append revert):
```go
// Create a revert commit that undoes the original
revertCommit := createRevertCommit(badCommit, message="Revert bad commit")
r.Storer.SetEncodedObject(revertCommit)
r.updateRef(revertCommit.Hash) // Move HEAD forward to revert
s.store.InsertCommit(ctx, taskID, revertCommit)
```

The history now shows: [original bad commit] → [revert commit]. Both are visible. Auditors can see what happened and why.

**Commit Chain Integrity**

Every commit references its parent via a cryptographic hash. The chain looks like:

```
Commit(A) → hash = H(content_A)
Commit(B) → parent = H(A), hash = H(content_B + parent)
Commit(C) → parent = H(B), hash = H(content_C + parent)
```

If someone tampers with Commit(A), its hash changes, which breaks Commit(B)'s parent reference, which breaks the chain.

Always verify the chain on read:

```go
func (s *CommitService) VerifyChain(ctx context.Context, from, to string) error {
    current := to
    for current != from {
        commit, _ := s.getCommit(ctx, current)
        parent, _ := s.getCommit(ctx, commit.Parent)

        if commit.Parent != parent.Hash {
            return fmt.Errorf("chain broken at %s", current)
        }

        current = commit.Parent
    }
    return nil
}
```

**Clock Injection (Time Determinism)**

Never call `time.Now()` directly in production code. Inject a clock function:

✗ FORBIDDEN:
```go
func (s *CommitService) CreateCommit(msg string) error {
    commit := object.NewCommit(object.CommitObject{
        Author: object.Signature{
            When: time.Now(), // Non-deterministic
        },
    })
}
```

✓ CORRECT:
```go
type CommitService struct {
    clock func() time.Time // Injected
}

func (s *CommitService) CreateCommit(msg string) error {
    commit := object.NewCommit(object.CommitObject{
        Author: object.Signature{
            When: s.clock(), // Deterministic and testable
        },
    })
}

// In main
svc := NewCommitService(store, repo, time.Now) // Production: real clock

// In tests
svc := NewCommitService(store, repo, func() time.Time {
    return time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC) // Predictable
})
```

**Error Wrapping (No Swallowed Errors)**

Every error must be wrapped with context. No error is ever silently dropped.

✗ FORBIDDEN (swallowed error):
```go
err := s.store.InsertCommit(ctx, taskID, commit)
// Silently ignore the error
```

✓ CORRECT (wrapped error):
```go
err := s.store.InsertCommit(ctx, taskID, commit)
if err != nil {
    return fmt.Errorf("insert commit: %w", err)
}
```

Use `fmt.Errorf` with `%w` to wrap. This preserves the error chain (readable with `errors.Unwrap`).

**Deterministic Operations**

Avoid randomness. If randomness is needed (e.g., generating IDs), use a seeded PRNG that is injected for testing.

✗ FORBIDDEN:
```go
id := uuid.New().String() // Non-deterministic
```

✓ CORRECT (with injection):
```go
type Service struct {
    idGen func() string
}

// Production
svc := NewService(func() string { return uuid.New().String() })

// Tests
svc := NewService(func() string { return "fixed-id-for-testing" })
```

---

### 9. Forbidden Patterns

These patterns are absolutely forbidden in mgit. They indicate a misunderstanding of the architecture or safety requirements. If you use any of these, your pull request will be rejected.

| Pattern | Why Forbidden | Correct Alternative |
|---------|---------------|-------------------|
| `exec.Command("git", ...)` | Non-deterministic, unauditable, unsafe | Use go-git plumbing API |
| `DELETE FROM task_commits` | Violates append-only principle | Use logical deletion with `deleted_at` timestamp |
| `UPDATE task_commits SET status = ?` | Rewrites history, breaks auditability | Append a new row; history is immutable |
| `time.Now()` in production code | Non-deterministic, untestable | Inject `func() time.Time` |
| `fmt.Sprintf` in SQL queries | SQL injection vulnerability | Use parameterized queries with `?` |
| Exposing `*git.Repository`, `*object.Commit` | Leaks implementation, breaks encapsulation | Wrap in domain types (model.Commit, etc.) |
| Global variables, `init()`, singletons | Non-testable, implicit dependencies | Dependency injection via constructors |
| `_ = fn()` (swallowed errors) | Hides failures, breaks error tracing | Wrap errors with `fmt.Errorf("context: %w", err)` |
| `:memory:` SQLite databases in tests | Data lost between test runs, non-deterministic | Use `t.TempDir()` for isolated temp databases |
| Hardcoded file paths | Breaks in different environments, untestable | Use config struct or injected path |
| Importing upward in dependency chain | Circular imports, architectural confusion | Refactor to move logic down (to lower layers) |

---

### 10. Anti-Stub Policy (CRITICAL)

#### 10.1 Stub Implementations Are NOT Done

A task is **NOT done** if the implementation contains any of the following patterns:

```go
// ❌ FORBIDDEN — printing "not implemented" and returning success
fmt.Println("X not yet implemented")
return nil

// ❌ FORBIDDEN — fake success response from API handler
c.JSON(200, map[string]string{"status": "completed", "message": "X completed"})
// ...but the handler does NO actual work

// ❌ FORBIDDEN — placeholder comment as implementation
// In a full implementation, this would:
// 1. Do X
// 2. Do Y
<-ctx.Done()
return nil

// ❌ FORBIDDEN — stub with integration pending comment
return SuccessResult("X requested (integration pending MGIT-N)")
```

#### 10.2 What To Do Instead

If the underlying implementation exists but CLI/API wiring is not done:
- **Wire it.** Call the real service/store method. That is the task.
- If wiring is blocked by missing infrastructure, mark the task as `"blocked"` with a clear reason.
- **NEVER** mark a task as `"done"` with a stub that compiles but does nothing.

If the feature is genuinely not implemented yet:
- Mark the task as `"open"` or `"blocked"`.
- Document what is missing in the task description.
- **NEVER** ship a function that returns fake success.

#### 10.3 Why This Matters

In safety-critical systems, a stub that returns success is worse than a function that returns an error. A fake `{"status": "completed"}` response from a squash handler deceives operators into believing the squash ran when it did not. A rollback command that prints "done" without actually reverting gives false confidence that code was reverted. A verify command that returns "chain intact" without checking the chain is an active lie to auditors.

These are not incomplete features — they are **active deceptions** that undermine system integrity.

#### 10.4 Verification

Before marking any task as done, grep for stub patterns:

```bash
grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"' \
  --include='*.go' --exclude='*_test.go'
```

If any matches are found in your changed files, the task is NOT done.

---

### 11. Pull Request Checklist

Before submitting work for review, verify every item on this checklist. If any item is unchecked, do not submit.

- [ ] **Tests pass**: `go test ./... -count=1` (run tests 1 time, not cached)
- [ ] **Race detector clean**: `go test ./... -race` (zero race conditions)
- [ ] **Coverage ≥ target**: Overall coverage ≥ 90%, specific packages per Section 4
- [ ] **Linting clean**: `golangci-lint run ./...` (zero warnings)
- [ ] **Vulnerabilities checked**: `govulncheck ./...` (clean output)
- [ ] **Build succeeds**: `go build -o mgit ./cmd/mgit/` (no compilation errors)
- [ ] **Task status updated**: `mtix_done` called for completed tasks (or `mtix_dep_add` to declare blockers)
- [ ] **Commit message**: Every commit references MGIT task ID (e.g., `Refs: MGIT-007`)
- [ ] **No forbidden patterns**: Reviewed code for all 10 patterns in Section 9
- [ ] **No stub implementations**: `grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"' --include='*.go' --exclude='*_test.go'` returns zero matches in changed files
- [ ] **Acceptance criteria met**: All criteria from task `acceptance` field are satisfied
- [ ] **go-git types wrapped**: No exported functions return `*git.Repository`, `*object.Commit`, etc.
- [ ] **SQLite parameterized**: All SQL queries use `?` placeholders, never string formatting
- [ ] **task_commits append-only**: No DELETE or UPDATE on task_commits table
- [ ] **FR/NFR references**: Godoc comments reference requirement IDs

---

### 12. Emergency Procedures

These procedures apply when something goes wrong. Follow them exactly.

**Security Vulnerability Discovered**

If you discover a security vulnerability (SQL injection risk, privilege escalation, data exposure, etc.):

1. **STOP immediately**. Do not commit vulnerable code.
2. **Document the vulnerability** in a comment or separate issue. Include: severity, affected component, reproduction steps, recommended fix.
3. **Do not push code that contains the vulnerability** to any branch.
4. **Notify the project maintainers** by raising a confidential issue (if the platform supports it) or emailing a maintainer directly.

Examples of vulnerabilities:
- SQL injection in a query (even if the input "looks safe")
- Exposure of go-git internals that allow external modification
- Race condition that could corrupt the commit index
- Timing attack that leaks information

**Data Corruption Detected**

If a test reveals data corruption (task_commits table has duplicates, commit chain is broken, etc.):

1. **STOP immediately**. Do not continue with the implementation.
2. **Do not assume it's a test bug.** Investigate the root cause in production code.
3. **Create a minimal reproduction** that exposes the corruption.
4. **Fix the root cause**, not the symptom. Often this is: missing transaction, missing lock, missing validation.
5. **Add a regression test** that would have caught this corruption.
6. **Document in code comments** what the bug was and why the fix works.

**Ambiguous or Missing Requirement**

If you are unsure about a requirement:

1. **Read REQUIREMENTS.md again**, focusing on the relevant FR/NFR section.
2. **Re-read the task context** by calling `mtix_context` with the task ID again.
3. **If still unclear**, do not guess. Instead:
   - Write a comment in the code or a documentation file explaining the ambiguity.
   - Commit this documentation.
   - Set the task status to `"blocked"` with a comment explaining what information is needed.

Example:
```
Task Status: blocked
Blocker: "Task MGIT-015 asks to squash commits but doesn't specify what
happens to commit metadata (agent_id, session_id). Should this be preserved
from the first commit? Last commit? Merged? Needs clarification."
```

**Unapproved Package Required**

If you need to add a dependency not on the approved packages list:

1. **STOP**. Do not add the dependency.
2. **Document your justification** in a comment and a separate issue/document.
3. **Follow PACKAGE-APPROVAL-PROCESS.md** to get the package approved before using it.

The approved list is in APPROVED-PACKAGES.md. It includes go-git, sqlite, testing libraries, and a few others. Any other package requires approval.

---

### 13. References

Refer to these documents for detailed guidance:

- **CLAUDE.md**: Operational constraints for Claude LLM agents (read first)
- **REQUIREMENTS.md**: Functional and non-functional requirements (FR/NFR numbers and definitions)
- **CODING-STYLE.md**: Go style guide, naming conventions, file organization
- **QUALITY-STANDARDS.md**: Testing standards, coverage expectations, code review criteria
- **TDD-WORKFLOW.md**: Test-driven development workflow, test structure, table-driven tests
- **APPROVED-PACKAGES.md**: List of approved dependencies and approval process
- **EXECUTION-PLAN.md**: High-level timeline and milestones for mgit development
- **mtix MCP tools**: Source of truth for all work items and their status (use `mtix_ready`, `mtix_context`, `mtix_claim`, `mtix_done`)

---

## Document Metadata

- **Version**: 1.0
- **Last Updated**: 2025-02-15
- **Audience**: LLM coding agents contributing to mgit
- **Classification**: Internal — Development Team

---

**Questions?**

If any part of this document is unclear, or if you need clarification on a requirement, raise an issue and set your task to "blocked" pending response. Guessing is not acceptable in safety-critical work.

Welcome to mgit. Code carefully.
