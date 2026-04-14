# mgit Quality Standards

**Version:** 1.0
**Status:** Active
**Last Updated:** 2026-03-09

mgit is a safety-critical micro version control system for LLM coding agents. This document establishes quality standards based on DO-178C, IEC 62304, NASA-STD-8739.8, MIL-STD-498, and OWASP ASVS Level 2.

---

## 1. Quality Philosophy

### Safety-Critical Foundation

mgit operates as a safety-critical system because data loss in mgit results in irreversible loss of the audit trail. Every commit, every merge, every state transition must be verifiable and recoverable. A commit atomicity flaw could introduce partial commits into production code, creating unpredictable system behavior. The consequences are not merely inconvenient—they are catastrophic.

**Core Principle:** Data loss is not recoverable. State corruption is not detectable after the fact. Therefore, every operation must be atomic, verifiable, and logged.

### Quality Mantra

> "If it's not tested, it doesn't work. If it's not documented, it doesn't exist."

This mantra governs all development in mgit:

- **Testing is prerequisite to existence.** Code without tests is not ready for review. Tests without coverage assertions are incomplete. Coverage metrics are enforced in CI; untested code blocks are failures.
- **Documentation is specification.** Godoc comments are not decorative. They describe the contract each function upholds. If a behavior is undocumented, it does not exist as a supported feature—it is an accident waiting to be fixed.
- **Correctness before performance.** Optimizations are acceptable only after correctness is proven. Premature optimization is the root of all bugs.

### The Append-Only Invariant

The most critical correctness property in mgit is the **append-only invariant**: the object store and audit log never delete, never overwrite, never rewind. This invariant must be enforced at three levels:

1. **File-level enforcement:** Audit logs are opened with `O_APPEND` flag. The filesystem kernel will not allow rewinding.
2. **Application-level enforcement:** All delete operations are rejected at application layer. Schema design prevents DELETE SQL statements.
3. **Verification layer:** Periodic integrity checks validate the append-only contract has never been violated.

If the append-only invariant is broken, mgit has failed its fundamental promise: to be a trustworthy audit trail.

---

## 2. Code Coverage Requirements

### Coverage Thresholds by Layer

mgit is organized into logical layers, each with distinct coverage requirements:

#### Store Layer (go-git wrapper + SQLite index)
- **Line coverage minimum:** 95%
- **Branch coverage minimum:** 90%
- **Rationale:** This layer is the foundation of all operations. Gaps here are gaps in every operation. go-git integration must be exhaustively tested because it is a boundary between managed and unmanaged code.

#### Service Layer (commit, squash, rollback)
- **Line coverage minimum:** 95%
- **Branch coverage minimum:** 90%
- **Rationale:** These are the high-value operations. Commit atomicity, squash idempotence, and rollback correctness are tested in detail here.

#### Model Layer (data structures, types)
- **Line coverage minimum:** 90%
- **Branch coverage minimum:** 85%
- **Rationale:** Model validation is critical but often simpler than service logic.

#### CLI Layer (command parsing, help text)
- **Line coverage minimum:** 90%
- **Branch coverage minimum:** 85%
- **Rationale:** CLI is user-facing but code-simple. Integration tests validate behavior; unit tests validate argument parsing.

#### API/MCP Layer (HTTP handlers, JSON marshaling)
- **Line coverage minimum:** 90%
- **Branch coverage minimum:** 85%
- **Rationale:** API handlers are thin wrappers. Most logic is tested at the service layer; API tests verify contract correctness.

#### Overall Minimum
- **Entire codebase:** 90% line coverage, 85% branch coverage
- **CI enforcement:** Coverage report is generated and compared against baseline. Any decrease triggers failure.

### Coverage Verification

Run the following command to measure and verify coverage:

```bash
make test-cover
```

This command:
1. Runs all tests with coverage instrumentation.
2. Generates `coverage.html` (browser-viewable report).
3. Outputs summary statistics to stdout.
4. Fails CI if overall coverage falls below 90% line / 85% branch.
5. Fails if any critical layer falls below its threshold.

---

## 3. Testing Requirements

### TDD Mandate: Red-Green-Refactor

All development follows strict Test-Driven Development (TDD):

1. **Red:** Write a failing test that describes the desired behavior.
2. **Green:** Write minimal code to make the test pass.
3. **Refactor:** Improve code quality without changing behavior.

No code review shall approve a change with failing tests. No feature shall merge without test coverage. TDD is not optional.

### Test Categories

mgit employs multiple test types, each serving distinct purposes:

#### Unit Tests
- Isolated function behavior with mocked dependencies.
- File: `*_test.go` in the same package.
- Example: `TestCommit_ValidMessage_CreatesCommitObject`.

#### Integration Tests
- Multiple components working together in-process.
- Use a real temporary directory store; mock external APIs.
- Example: `TestCommitAndLogQuery_AppendedToIndex`.

#### End-to-End Tests
- Full workflow via public API or CLI.
- Spin up a real mgit instance; exercise multiple operations.
- Example: `TestE2E_Commit_Squash_Rollback_ConsistentState`.

#### Property-Based Tests
- Use `github.com/lestrrat-go/quickcheck` or `pgk.go.dev/testing/quick`.
- Verify invariants hold over random inputs.
- Example: `TestPropertyCommitIDsAreUnique`.

#### Fuzz Tests
- Use Go's native fuzzing (`go test -fuzz`).
- Test parsers and serializers with malformed inputs.
- Example: `FuzzParseCommitMessage`.

### mgit-Specific Test Scenarios

The following scenarios must be exhaustively tested because they are where correctness is most fragile:

#### Commit Atomicity
- Test that a commit fails completely or succeeds completely; no partial state.
- Test that if the index write fails, the object store is not modified.
- Test that if the filesystem is full mid-write, the store recovers to a clean state.

#### Squash Idempotence
- Test that `Squash(commits, strategy)` produces the same result when called twice.
- Test that squashing once, rolling back, and squashing again is idempotent.
- Test that squashing interleaved with other commits maintains consistency.

#### Rollback Correctness
- Test that `Rollback(targetCommit)` returns the store to the exact state before that commit.
- Test that rollback's audit log entry is itself immutable (cannot be rolled back).
- Test that rolling back a commit in the middle of a chain leaves the chain intact.

#### Append-Only Enforcement
- Test that `Delete()` operations are rejected at the service layer.
- Test that SQL delete/update statements are never executed.
- Test that filesystem-level delete attempts fail (via mocked fs or real disk tests).

#### Branch Safety
- Test that creating a branch creates no orphaned commits.
- Test that merging branches produces clean merge history (no loops, no dangling references).
- Test that switching branches correctly updates the working directory state.
- Test that concurrent branch operations do not corrupt the branch index.

#### Task Mapping Consistency
- Test that bidirectional task-to-commit queries always return consistent results.
- Test that `GetCommitsByTaskID(taskID)` and `GetTaskIDsByCommitID(commitID)` are inverses.
- Test that updating task mapping leaves the commit itself unchanged.

### Test Isolation

Each test runs in isolation:
- Every test receives a unique temporary directory created by `ioutil.TempDir()`.
- The test setup initializes a fresh mgit store in that directory.
- The test teardown removes the directory (and verifies no lingering handles).
- Tests run in parallel by default; use `t.Parallel()` at the start of test functions.
- Shared state is forbidden; if tests interfere, they are rewritten.

### Test Naming Convention

All tests follow a consistent naming scheme:

```
Test{FunctionName}_{Scenario}_{ExpectedOutcome}
```

Examples:
- `TestCommit_ValidMessage_CreatesCommitObject`
- `TestCommit_EmptyMessage_ReturnsError`
- `TestSquash_TwoCommits_ProducesOneCommit`
- `TestRollback_TargetInMiddle_RestoresExactState`

This naming scheme makes test intent immediately obvious and enables easy search and maintenance.

---

## 4. Static Analysis

### golangci-lint

All Go code is analyzed by `golangci-lint` with the following strict configuration:

```yaml
linters:
  enable:
    - asasalint
    - asciicheck
    - bidichk
    - bodyclose
    - containedctx
    - contextcheck
    - copyloopvar
    - cyclop
    - decorder
    - depguard
    - dogsled
    - dupl
    - dupword
    - durationcheck
    - errchkjson
    - errname
    - errorlint
    - exhaustive
    - exportloopref
    - forbidigo
    - forcetypeassert
    - funlen
    - gci
    - gochecknoglobals
    - gochecknoinits
    - gocognit
    - gocritic
    - gocyclo
    - godot
    - godox
    - goerr113
    - gofmt
    - gofumpt
    - goheader
    - goimports
    - golint
    - gomnd
    - gomoddirectives
    - gomodguard
    - goprintffuncname
    - gosec
    - gosimple
    - gosmopolitan
    - govet
    - grouper
    - ineffassign
    - interfacebloat
    - ireturn
    - lll
    - loggercheck
    - maintidx
    - makezero
    - misspell
    - musttag
    - nakedret
    - nilerr
    - nilnil
    - noctx
    - nolintlint
    - nonamedreturns
    - nosprintfhostport
    - nstmt
    - paralleltest
    - perfsprint
    - prealloc
    - predeclared
    - promlinter
    - protogetter
    - reassign
    - revive
    - rowserrcheck
    - sloglint
    - spancheck
    - sqlc
    - staticcheck
    - stdlib
    - stropping
    - stylecheck
    - tagliatelle
    - tenv
    - testableexamples
    - testinggoroutine
    - testpackage
    - thelper
    - tparallel
    - typecheck
    - unconvert
    - unparam
    - unreachable
    - unsafeptr
    - unusedexcept
    - unusedwrite
    - usestdlibvars
    - varnamelen
    - wastedassign
    - whitespace
    - wsl
```

Run analysis with:

```bash
golangci-lint run ./...
```

### gosec Security Scanner

All code is scanned by `gosec` with all rules enabled:

```bash
gosec -tests ./...
```

gosec checks for:
- Hardcoded credentials and secrets.
- Unsafe cryptography (weak hashing, bad random number generation).
- SQL injection vulnerabilities.
- Command injection vulnerabilities.
- Insecure deserialization.

A single gosec warning in CI is a failure.

### govulncheck

All dependencies are checked for known vulnerabilities:

```bash
govulncheck ./...
```

This command queries the Go vulnerability database and fails if any transitive dependency has a known CVE. Vulnerable dependencies must be upgraded or replaced.

### Complexity Limits

- **Cyclomatic complexity:** ≤15 per function. Measured by `gocyclo`.
- **Cognitive complexity:** ≤20 per function. Measured by `gocognit`.

Functions exceeding these thresholds are refactored before merge. Complex logic is split into smaller, testable functions.

### Zero-Warning Policy

CI enforces a zero-warning policy:
- Any linter warning is a build failure.
- Any gosec finding is a build failure.
- Any vulnerability in govulncheck is a build failure.
- Developers must resolve every issue, not suppress it (suppressions are reserved for false positives and must be approved).

---

## 5. Security Requirements

### Cryptographic Hashing

- **Approved algorithm:** SHA-256 (NIST FIPS 180-4).
- **Forbidden algorithms:** MD5, SHA-1, any algorithm with known collisions.
- **Application:** All commit IDs, object IDs, and integrity checks use SHA-256.
- **Verification:** `gosec` and code review ensure no weak hashing is used.

### SQL Injection Prevention

- **Mandate:** All SQL queries use parameterized statements with `?` placeholders.
- **Forbidden:** String interpolation, string concatenation, or fmt.Sprintf in SQL queries.
- **Tool:** golangci-lint and `sqlc` validate this at compile time.
- **Example (correct):**
  ```go
  query := "SELECT * FROM commits WHERE task_id = ?"
  rows, err := db.Query(query, taskID)
  ```
- **Example (forbidden):**
  ```go
  query := fmt.Sprintf("SELECT * FROM commits WHERE task_id = '%s'", taskID)
  rows, err := db.Query(query)  // SECURITY FAILURE
  ```

### Input Validation

All user inputs are validated before use:

- **Task IDs:** Alphanumeric only; no special characters; length 1–64 characters.
- **File paths:** No absolute paths; no `../` sequences; must be relative to store root.
- **Commit messages:** UTF-8 encoded; max 10,000 characters; no null bytes.
- **Branch names:** Alphanumeric, hyphen, underscore; no spaces; length 1–128 characters.

Validation is performed in model constructors and service entry points. Invalid inputs return `ErrInvalidInput` with the specific field in the error message.

### Path Traversal Prevention

- All file paths are validated with `filepath.Clean()` and checked for `..` sequences.
- All paths are verified to be within the store root directory (using `filepath.EvalSymlinks` and comparison).
- No symlinks are followed outside the store root.
- Tests verify that inputs like `../../etc/passwd` are rejected.

### API Security

- **Default behavior:** The API listens on `localhost:9999` only (not `0.0.0.0`).
- **TLS:** Strongly recommended for production; configuration is documented.
- **Authentication:** API requires a pre-shared token passed via `Authorization: Bearer {token}` header.
- **CORS:** Disabled by default (no `Access-Control-Allow-Origin` header sent). When enabled via `api.cors_enabled=true`, restrict allowed origins to localhost only (`http://localhost:*`, `http://127.0.0.1:*`). Non-localhost origins require explicit configuration via `api.cors_origins`.

### Audit Log Immutability

The audit log is immutable at the filesystem level:

- Opened with `os.OpenFile(path, os.O_APPEND | os.O_WRONLY | os.O_CREATE, 0o644)`.
- The kernel O_APPEND flag ensures all writes go to the end; seeking to earlier offsets is impossible.
- Periodic verification scans the log for any violation of append-only ordering.
- If any violation is detected, the store enters a read-only mode and alerts the operator.

---

## 6. Performance Benchmarks

mgit performance is measured against these benchmarks. All benchmarks are run with `go test -bench=. -benchmem -count=5` to ensure results are stable and representative.

### Benchmark Thresholds

| Benchmark | Operation | Threshold | Notes |
|-----------|-----------|-----------|-------|
| BenchmarkCommit | Single commit creation | <5ms | Includes index update |
| BenchmarkLogQuery_ByTaskID | Query 10K commits by task ID | <50ms | Index must be used; full scan is failure |
| BenchmarkSquash_100Commits | Squash 100 commits into one | <500ms | Includes verification |
| BenchmarkDiff_1000Files | Diff two commits with 1000 file changes | <100ms | May be filesystem-dependent |
| BenchmarkVerify_10KCommits | Full integrity check of 10K commits | <1s | Verifies all hashes and references |
| BenchmarkStartup | Load existing store with 1K commits | <50ms | From disk to ready state |

### Running Benchmarks

```bash
go test ./... -bench=. -benchmem -count=5
```

Output includes:
- Operations per second (higher is better).
- Bytes allocated per operation.
- Garbage collection statistics.

Benchmarks are run before and after performance-sensitive changes. Any regression requires justification and approval from the reviewer.

### Benchmark Regression Policy

- Benchmarks are committed to the repository (see `*_test.go` files).
- CI runs benchmarks on every PR and compares against the baseline branch.
- Any regression >5% is flagged and must be explained.
- Any improvement >10% is celebrated but not required.

---

## 7. Documentation Requirements

### Godoc Coverage

Every exported type, function, and constant must have a Godoc comment:

```go
// Commit represents a version control snapshot of the codebase.
//
// A Commit is immutable once created. It contains a list of files,
// their contents, and metadata such as the author and timestamp.
type Commit struct {
    ID        string
    Message   string
    TaskID    string
    Timestamp time.Time
}

// CommitOptions configures the behavior of a Commit operation.
type CommitOptions struct {
    // Message is the human-readable commit message.
    Message string

    // TaskID optionally associates this commit with a task.
    TaskID string
}

// CreateCommit creates a new commit from the given files and options.
//
// If the commit cannot be written to the object store, an error is returned.
// The commit is atomic: either it succeeds completely or fails completely.
//
// Errors:
//   - ErrInvalidMessage: message is empty or exceeds max length
//   - ErrInvalidTaskID: task ID contains invalid characters
//   - ErrStorageFull: filesystem is out of space
//   - ErrPermissionDenied: store directory is not writable
func (s *Store) CreateCommit(ctx context.Context, opts CommitOptions) (*Commit, error) {
    // ...
}
```

### Inline Comments for Complex Logic

Complex algorithms, especially those involving go-git integration, are documented with inline comments:

```go
// Merge uses the three-way merge algorithm:
// 1. Find the common ancestor of base and head.
// 2. Apply base->head changes to base.
// 3. Apply base->merge changes to the result.
// 4. If conflicts exist, mark them and return ErrMergeConflict.
func (s *Store) Merge(base, head, merge string) (string, error) {
    ancestor, err := s.findCommonAncestor(base, merge)
    if err != nil {
        return "", err
    }
    // ...
}
```

### Commit Data Model Documentation

The following must be documented in the package documentation (package comment):

- The structure of a commit object in the object store.
- The format of the index file (SQLite schema).
- The format of the audit log.
- The invariants that are always true (e.g., append-only).

Example:

```go
/*
Package mgit provides version control operations for LLM coding agents.

# Data Model

A Commit object is stored as JSON in the object store with the following structure:

	{
	  "id": "sha256:abc123...",
	  "message": "Fix authentication bug",
	  "taskID": "task-42",
	  "timestamp": "2026-03-09T15:30:00Z",
	  "author": "claude-opus-4-6",
	  "files": {
	    "src/auth.go": {
	      "hash": "sha256:def456...",
	      "size": 2048
	    }
	  }
	}

# Index Schema

The index is an SQLite database with the following tables:

- commits: (id, message, task_id, timestamp, author)
- commit_files: (commit_id, file_path, file_hash)
- branch_refs: (branch_name, commit_id)

# Audit Log

The audit log is append-only and contains one line per operation:

	2026-03-09T15:30:00Z CREATE_COMMIT sha256:abc123... task-42

# Invariants

The following invariants are always maintained:

- Append-only: The object store and audit log never delete or overwrite data.
- Consistency: Every commit in the index has a corresponding object in the store.
- Atomicity: An operation either succeeds entirely or fails entirely; no partial state.
*/
package mgit
```

### Error Code Documentation

All error types are documented with their meanings:

```go
var (
    // ErrNotFound is returned when a requested commit or object does not exist.
    ErrNotFound = errors.New("object not found")

    // ErrInvalidMessage is returned when a commit message is empty or exceeds limits.
    ErrInvalidMessage = errors.New("commit message is invalid")

    // ErrMergeConflict is returned when a merge operation encounters conflicts.
    // The caller may inspect the merge output to see conflicting regions.
    ErrMergeConflict = errors.New("merge conflict")

    // ErrAppendOnlyViolation is returned if the append-only invariant is violated.
    // This indicates data corruption and the store should be taken offline.
    ErrAppendOnlyViolation = errors.New("append-only invariant violated")
)
```

---

## 8. Quality Gates and CI/CD Integration

### Continuous Integration Checklist

Every commit must pass the following before merge:

1. **Tests:** `go test ./...` passes with no failures.
2. **Coverage:** `make test-cover` reports ≥90% line, ≥85% branch coverage.
3. **Linting:** `golangci-lint run ./...` reports zero warnings.
4. **Security:** `gosec -tests ./...` reports zero findings.
5. **Vulnerabilities:** `govulncheck ./...` reports zero CVEs.
6. **Benchmarks:** Performance does not regress >5% on critical paths.
7. **Build:** `go build ./...` succeeds on all supported platforms.

### Code Review

All changes require approval from at least one other team member before merge. Reviewers check:

- Test coverage is sufficient and scenarios are complete.
- Godoc comments are present and accurate.
- Security practices are followed (no hardcoded secrets, parameterized SQL, etc.).
- Complexity limits are met.
- Performance benchmarks are not degraded.
- Append-only invariant is preserved.

### Release Process (IEC 62304 §5.8)

Before release:

1. All tests pass in CI (zero failures, zero race conditions).
2. Coverage report is reviewed and targets are met (90%+ line, 85%+ branch).
3. CHANGELOG is updated with commit summaries referencing MGIT task IDs.
4. Commit history is audited for security issues (gosec + govulncheck clean).
5. Performance benchmarks are run and documented (no >5% regression).
6. Release notes are written and reviewed with known issues section.
7. `mgit docs generate` is run to ensure agent documentation is current.
8. Requirements Traceability Matrix (RTM) reviewed — no "UNTESTED" requirements.
9. Binary is built with `CGO_ENABLED=0 go build` and smoke-tested on target platforms.
10. Release candidate is tagged, signed, and SHA-256 checksums published.

**Sign-off required:** At least one reviewer must sign off on the release checklist before publishing. The sign-off is recorded in the CHANGELOG with reviewer name and date.

### Software Maintenance Plan (IEC 62304 §6)

Post-release maintenance follows these procedures:

**Bug Fix Process:**
1. Bug reported → create mgit-tasks.json entry with severity assessment
2. Security bugs: patch within 72 hours, advisory within 24 hours
3. Critical bugs: patch within 1 week
4. Non-critical bugs: patch in next scheduled release

**Security Patch Policy:**
- govulncheck run weekly against all dependencies
- CVE in direct dependency: patch within 72 hours
- CVE in transitive dependency: evaluate impact, patch within 1 week
- Security advisories published in SECURITY.md

**Deprecation Procedure:**
- Features deprecated with 2 minor version warning period
- Deprecated features log warnings when used
- Deprecated features removed in the next major version

### Software Metrics (NASA-STD-8739.8)

mgit collects and reports the following quality metrics:

**Per-Release Metrics:**
- Test count (unit, integration, E2E)
- Code coverage (line and branch, per-package)
- Defect density (bugs per 1000 lines of code)
- Static analysis findings (golangci-lint warning count over time)
- Dependency count (direct and transitive)
- Binary size

**Per-CI-Run Metrics:**
- Test pass rate
- Coverage delta from baseline
- Benchmark regression delta
- Build time

Metrics are exported via `mgit metrics --format json` to a file at `.mgit/metrics.json`. This command is included in the CI pipeline and tracked over time. Trend analysis is performed before each release to identify quality regression patterns.

---

## 9. Compliance Matrix

This document ensures mgit complies with the following standards:

| Standard | Requirement | Implementation |
|----------|-------------|-----------------|
| DO-178C | Objective 1: Software Requirements | REQUIREMENTS.md (15 FRs, 5 NFRs) |
| DO-178C | Objective 2: Software Design | CODING-STYLE.md, ADR-001, ADR-002 |
| DO-178C | Objective 3: Implementation | TDD Mandate + Code Review |
| DO-178C | Objective 4: Verification | Unit Tests + Integration Tests + MC/DC for 3 critical functions (ADR-003) |
| DO-178C | Objective 5: Traceability | Auto-generated RTM via FR-15.10 |
| DO-178C | Tool Qualification | TQL-3 per ADR-003 (output reviewed before production use) |
| IEC 62304 | §5.8 Release | Release Process checklist (this document) |
| IEC 62304 | §6 Maintenance | Software Maintenance Plan (this document) |
| IEC 62304 | Risk Management | Security Requirements Section, CLAUDE.md failure scenarios |
| IEC 62304 | Configuration Management | Version Control with mgit itself |
| IEC 62304 | Problem Resolution | Issue Tracking + Commit Log |
| NASA-STD-8739.8 | Code Quality | Static Analysis + Linting (80+ rules) |
| NASA-STD-8739.8 | Testing Coverage | Code Coverage Requirements (90%/85%) |
| NASA-STD-8739.8 | Metrics Collection | Software Metrics section (this document) |
| MIL-STD-498 | Documentation | Godoc + Inline Comments + FR-15 agent docs |
| MIL-STD-498 | Configuration Management | git + audit log |
| MIL-STD-498 | Traceability Matrix | Auto-generated RTM (FR-15.10) |
| OWASP ASVS L2 | V2 Authentication | Bearer token with lifecycle (NFR-5.11) |
| OWASP ASVS L2 | V5 Input Validation | Input Validation Section |
| OWASP ASVS L2 | V6 Cryptography | Dual-hash model (ADR-002, NFR-5.1) |
| OWASP ASVS L2 | V7 Error Handling | Error Code Documentation |
| OWASP ASVS L2 | V9 Communication | Mandatory TLS for non-localhost (NFR-5.9) |
| OWASP ASVS L2 | V13 API Security | Rate limiting, size limits (NFR-5.10) |

---

## 10. Revision History

| Version | Date | Author | Change |
|---------|------|--------|--------|
| 1.0 | 2026-03-09 | Safety Team | Initial quality standards |

---

## Appendix A: Quick Reference Commands

```bash
# Run all tests
go test ./...

# Run tests with coverage
make test-cover

# Run linters
golangci-lint run ./...

# Run security scanner
gosec -tests ./...

# Check dependencies for vulnerabilities
govulncheck ./...

# Run benchmarks
go test ./... -bench=. -benchmem -count=5

# Build the project
go build ./...

# Format code
gofmt -w .

# Generate Godoc
go doc ./...
```

---

**END OF DOCUMENT**
