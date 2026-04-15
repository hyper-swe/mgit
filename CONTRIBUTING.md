# Contributing to mgit

Thank you for your interest in contributing to mgit! This guide covers the development workflow, coding standards, and conventions used in the project.

## Getting Started

### Prerequisites

- Go 1.22 or later
- golangci-lint
- govulncheck

### Setup

```bash
git clone https://github.com/hyper-swe/mgit.git
cd mgit
go mod download
go build -o mgit ./cmd/mgit/
```

### Running Tests

```bash
# Unit tests
go test ./... -count=1

# With race detector
go test ./... -race -count=1

# With coverage
go test ./... -coverprofile=cover.out -count=1
go tool cover -func=cover.out | tail -1
```

## Use mtix to Track mgit Development

mgit uses [mtix](https://github.com/hyper-swe/mtix) for task management. All work items are tracked as mtix nodes.

### Finding Work

```bash
mtix ready                    # List unblocked, unclaimed tasks
mtix show {TASK-ID}           # View task details
mtix context {TASK-ID}        # Get full ancestor context chain
```

### Task Workflow

1. Find an available task with `mtix ready`
2. Read the task context with `mtix context {TASK-ID}`
3. Claim the task with `mtix claim {TASK-ID}`
4. Implement following TDD workflow (see below)
5. Mark complete with `mtix done {TASK-ID}`

### Commit Messages Reference Tasks

```
feat(service): implement squash with atomic rollback

Refs: {TASK-ID}
```

## Test-Driven Development

TDD is non-negotiable. Every change follows this sequence:

```
READ spec -> WRITE test (red) -> RUN test (fails) -> WRITE code -> RUN test (green) -> REFACTOR -> CHECK coverage -> COMMIT
```

### Test Naming

```go
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {}
func TestSquashTask_AtomicFailure_RollsBackAllChanges(t *testing.T) {}
func TestRollback_AppendOnly_CreatesRevertCommit(t *testing.T) {}
```

### Table-Driven Tests

Use table-driven tests for functions with multiple input/output scenarios:

```go
tests := []struct {
    name    string
    input   string
    wantErr bool
}{
    {"valid_input", "TASK-1", false},
    {"empty_input", "", true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        // test body
    })
}
```

### Coverage Requirements

| Layer | Line Coverage | Branch Coverage |
|-------|---------------|-----------------|
| Store (go-git + SQLite) | 95% | 90% |
| Service (Commit, Squash, Rollback) | 95% | 90% |
| Model / CLI / API / MCP | 90% | 85% |
| **Overall** | **90% min** | **85% min** |

## Architecture

mgit uses a layered architecture with strict dependency rules:

```
+-------------------------------------------+
| CLI Commands / REST Handlers / MCP Tools  |
+---------------------+---------------------+
                      |
               +------v------+
               | Service     |  <-- All business logic
               | (Commit,    |
               |  Squash,    |
               |  Rollback)  |
               +------+------+
                      |
               +------v-----------+
               | Store Interfaces |  <-- Data access contracts
               +------+-----------+
                     / \
                    /   \
          +--------+   +---------+
          | go-git |   | SQLite  |
          | Store  |   | Index   |
          +--------+   +---------+
```

### Dependency Rules

- **Handlers never access Store directly** -- always go through Service
- **Store layer** = data access only (CRUD + transactions)
- **Service layer** = business logic (Commit, Squash, Rollback semantics)
- **Model layer** = types and errors (depends on nothing except stdlib)
- **Dependency direction:** `cmd/ -> service/ -> store/ -> model/`

## SQL Rules

These are strict rules, not suggestions.

### Parameterized Queries Only

```go
// Correct
row := db.QueryRow("SELECT id FROM commits WHERE task_id = ?", taskID)

// Forbidden -- SQL injection risk
row := db.QueryRow(fmt.Sprintf("SELECT id FROM commits WHERE task_id = '%s'", taskID))
```

### PRAGMAs on Every Connection

```go
db.Exec("PRAGMA foreign_keys = ON")
db.Exec("PRAGMA journal_mode = WAL")
db.Exec("PRAGMA busy_timeout = 5000")
db.Exec("PRAGMA synchronous = FULL")
```

### All Writes in Transactions

All write operations must be wrapped in a transaction for atomicity.

### Append-Only Audit Tables

The `task_commits` table is append-only. No UPDATE or DELETE operations are permitted. Audit compliance requires complete history.

## go-git Rules

### Never Shell Out to git

```go
// Forbidden
exec.Command("git", "commit", "-m", msg).Run()

// Correct -- use go-git API
worktree.Commit(msg, &git.CommitOptions{Author: &object.Signature{}})
```

### Wrap go-git Types

Do not expose go-git types directly. Use mgit's own model types and convert as needed.

## Security

- Treat all input as hostile (CLI flags, REST payloads, MCP tool inputs)
- Use parameterized SQL queries exclusively
- Bind to localhost by default
- Content addressing prevents tampering (SHA-1 from git, SHA-256 from mgit)
- Only use approved packages listed in APPROVED-PACKAGES.md

## Naming Conventions

### Errors

```go
var ErrCommitNotFound = errors.New("commit not found")   // Correct
var CommitNotFoundErr = errors.New("commit not found")   // Wrong
```

### JSON Tags

Use snake_case for JSON field names:

```go
type CommitResponse struct {
    TaskID     string `json:"task_id"`
    CommitHash string `json:"commit_hash"`
    CreatedAt  string `json:"created_at,omitempty"`
}
```

### Timestamps

Always use ISO-8601 UTC:

```go
timestamp := time.Now().UTC().Format(time.RFC3339)
```

### Import Organization

Group imports in three blocks separated by blank lines:

1. Standard library
2. Third-party packages
3. Internal packages

## Commit Message Format

```
{type}({scope}): {description}

{body}

Refs: {TASK-ID}
```

### Types

| Type | Use |
|------|-----|
| `feat` | New feature |
| `fix` | Bug fix |
| `test` | Test coverage |
| `refactor` | Code reorganization (no behavior change) |
| `docs` | Documentation or comments |
| `perf` | Performance improvement |
| `chore` | Tooling, CI, dependencies |

### Scopes

| Scope | Area |
|-------|------|
| `store` | SQLite or go-git store layer |
| `service` | Commit, Squash, or Rollback service |
| `cli` | Command-line interface |
| `api` | REST handlers |
| `mcp` | MCP tool integration |
| `model` | Types and errors |
| `git` | go-git wrapper |

## Code Limits

| Metric | Limit |
|--------|-------|
| Function body | 50 lines |
| Cyclomatic complexity | 15 |
| Parameters per function | 5 |
| File length | 500 lines |

## Static Analysis

All of the following must pass with zero warnings before submitting a PR:

```bash
go test ./... -race -count=1
golangci-lint run ./...
go vet ./...
govulncheck ./...
```

## Submitting Changes

1. Fork the repository
2. Create a feature branch from `main`
3. Follow TDD workflow for all changes
4. Ensure all tests pass and coverage meets thresholds
5. Run static analysis (zero warnings)
6. Write a clear commit message referencing the task ID
7. Open a pull request with a description of the change

## License

By contributing, you agree that your contributions will be licensed under the same license as the project.
