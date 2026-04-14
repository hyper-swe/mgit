# mgit Coding Style Guide

mgit (micro git — a safety-critical micro version control system for LLM coding agents) is a companion to mtix (micro-tix). This guide establishes mandatory coding standards for all contributors. Adherence is non-negotiable for safety-critical functionality.

---

## 1. Project Layout

The project uses a layered hexagonal architecture with strict dependency boundaries.

```
mgit/
├── cmd/
│   └── mgit/
│       ├── main.go
│       ├── commands/
│       │   ├── commit.go      # CLI: mgit commit
│       │   ├── squash.go      # CLI: mgit squash
│       │   ├── rollback.go    # CLI: mgit rollback
│       │   ├── branch.go      # CLI: mgit branch
│       │   ├── diff.go        # CLI: mgit diff
│       │   ├── log.go         # CLI: mgit log
│       │   ├── verify.go      # CLI: mgit verify
│       │   └── audit.go       # CLI: mgit audit
│       └── root.go            # Cobra root command
│
├── internal/
│   ├── model/
│   │   ├── errors.go          # Sentinel errors: ErrCommitNotFound, etc.
│   │   ├── commit.go          # Commit, CommitMetadata, CommitProof
│   │   ├── branch.go          # Branch, BranchRef
│   │   ├── taskid.go          # TaskID type, validation
│   │   ├── filediff.go        # FileDiff, DiffStats
│   │   └── constants.go       # MaxTaskIDDepth=50, SHALength=64, etc.
│   │
│   ├── store/
│   │   ├── git/
│   │   │   ├── repository.go  # Repository wrapper (open, clone, init)
│   │   │   ├── commit.go      # CommitStore: create, get, list, tree
│   │   │   ├── branch.go      # BranchStore: create, get, switch, delete
│   │   │   ├── diff.go        # DiffStore: diff, stats
│   │   │   ├── object.go      # object helpers (Blob, Tree, Commit)
│   │   │   ├── tree.go        # Tree building, traversal
│   │   │   └── worktree.go    # Worktree: checkout, status, clean
│   │   │
│   │   └── index/
│   │       ├── schema.go      # SQLite schema migration
│   │       ├── store.go       # IndexStore: open, close, migration
│   │       ├── task_commits.go # task_commits table operations
│   │       ├── branches.go    # branches table operations
│   │       └── migration.go   # Versioned schema migrations
│   │
│   ├── service/
│   │   ├── commit.go          # CommitService: create, verify
│   │   ├── squash.go          # SquashService: squash, verify chain
│   │   ├── rollback.go        # RollbackService: rollback, audit
│   │   ├── branch.go          # BranchService: create, switch, list
│   │   ├── diff.go            # DiffService: diff, stats
│   │   ├── verify.go          # VerifyService: verify commits, integrity
│   │   ├── audit.go           # AuditService: log, query audit trail
│   │   └── config.go          # ConfigService: load, validate, get
│   │
│   ├── api/
│   │   ├── http/
│   │   │   ├── handler.go     # HTTP handlers
│   │   │   ├── commit.go      # POST /commits
│   │   │   ├── branch.go      # GET/POST /branches
│   │   │   ├── diff.go        # GET /diffs
│   │   │   ├── task.go        # GET /tasks/:taskID
│   │   │   ├── middleware.go  # Logging, error handling
│   │   │   └── router.go      # Echo router setup
│   │   └── types.go           # Request/response DTOs
│   │
│   ├── mcp/
│   │   ├── server.go          # MCP server (cwd, capability negotiation)
│   │   ├── tools.go           # Tool implementations
│   │   └── types.go           # MCP request/response types
│   │
│   ├── mtix/
│   │   ├── client.go          # mtix client wrapper
│   │   └── integration.go     # task-commit synchronization
│   │
│   └── testutil/
│       ├── fixtures.go        # Test commit, branch, tree builders
│       ├── sqlite.go          # SQLite test database setup
│       └── clock.go           # Mock clock for time injection
│
├── e2e/
│   ├── commit_test.go
│   ├── squash_test.go
│   ├── rollback_test.go
│   ├── branch_test.go
│   └── fixtures.go
│
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── CODING-STYLE.md (this file)
```

### Directory Principles

- **Flat internal structure**: Package names lowercase, no nesting beyond 2 levels
- **One concept per file**: Commit creation in one file, squashing in another
- **Max 500 lines per file**: Split large files (e.g., store.go → schema.go, operations.go)
- **cmd/ minimal**: 10-20 line command files; defer to services
- **No middleware in cmd/**: Business logic lives in service/, not CLI
- **Test files colocated**: `foo.go` and `foo_test.go` in same package

---

## 2. Naming Conventions

### Package Names

- **model**: Core types (Commit, Branch, TaskID, FileDiff)
- **store/git**: go-git wrappers and repository operations
- **store/index**: SQLite task-commit index
- **service**: Business logic and orchestration
- **api**: HTTP handlers and DTOs
- **mcp**: MCP server and tools
- **testutil**: Test helpers and fixtures

All lowercase, concise, no underscores.

### Type Names

```go
// Domain types: PascalCase, domain-specific
type Commit struct {
    ID        string    // SHA-256
    ParentID  string    // SHA-256
    AuthorID  string    // LLM agent ID
    Timestamp time.Time
    Message   string
    TreeID    string    // root tree hash
    Metadata  CommitMetadata
}

type CommitMetadata struct {
    TaskID       model.TaskID // PROJ-4.2.1
    Agent        string       // Agent name
    TokenCount   int          // Tokens used
    ModelVersion string       // Claude 3.5 Sonnet, etc.
}

type SquashRequest struct {
    TaskID      model.TaskID
    IntoParent  bool      // Squash into parent?
    KeepTags    []string  // Preserve commit messages
}

type RollbackRequest struct {
    TaskID       model.TaskID
    TargetCommit string // SHA to rollback to
    Force        bool   // Ignore uncommitted changes
}

type Branch struct {
    Name      string // "main", "agent-X-session-Y"
    CommitID  string // HEAD commit
    IsHead    bool
    CreatedAt time.Time
}

type TaskID struct {
    Value string // "PROJ-4.2.1"
}

type FileDiff struct {
    Path      string   // "src/main.go"
    Mode      FileMode // Added, Modified, Deleted
    OldSize   int64
    NewSize   int64
    Lines     int      // +/- line count
    Hunks     []Hunk
}

type DiffStats struct {
    FilesChanged int
    Insertions   int
    Deletions    int
    TotalLines   int
}
```

### Error Types

Sentinel errors in `model/errors.go`. Exported, uppercase with `Err` prefix:

```go
var (
    ErrCommitNotFound     = errors.New("commit not found")
    ErrTaskNotFound       = errors.New("task not found")
    ErrBranchNotFound     = errors.New("branch not found")
    ErrInvalidTaskID      = errors.New("invalid task ID format")
    ErrLockContention     = errors.New("database lock contention")
    ErrChainBroken        = errors.New("commit chain broken or orphaned")
    ErrSquashFailed       = errors.New("squash operation failed")
    ErrRollbackConflict   = errors.New("rollback has conflicts")
    ErrDiffGenerationFailed = errors.New("diff generation failed")
    ErrVerificationFailed = errors.New("commit verification failed")
)
```

### Constants

```go
const (
    MaxTaskIDDepth   = 50        // PROJ-1.2.3... max depth
    SHALength        = 64        // SHA-256 hex string
    ShortIDLength    = 8         // Short commit ID
    MaxCommitMessage = 5000      // bytes
    MaxBranchName    = 255       // characters
    TaskIDPattern    = "^[A-Z]+(-[A-Z0-9]+)*(\\.\\d+)*$" // Regex
)
```

### Function Names

- **Verb-noun**: `CreateCommit`, `SquashTask`, `GetBranch`, `ListCommits`
- **Boolean getters**: `IsHead`, `HasConflicts`, `IsSafe`
- **Test functions**: `Test{Function}_{Scenario}_{Expected}`

Examples:

```go
// ✓ Good
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*Commit, error)
func (s *SquashService) SquashTask(ctx context.Context, taskID model.TaskID) (string, error)
func (s *VerifyService) VerifyCommit(ctx context.Context, commitID string) error
func (b *Branch) IsHead() bool

// ✗ Bad
func (s *CommitService) Commit(ctx context.Context, ...) // Unclear what action
func (s *SquashService) Handle(...) // Too generic
func GetCommitID() // No receiver context
```

### Test Function Names

```go
// Format: Test{FunctionUnderTest}_{Scenario}_{Expected}
func TestCreateCommit_ValidRequest_CreatesCommitObject(t *testing.T)
func TestSquashTask_MultipleCommits_ProducesSignedCommit(t *testing.T)
func TestVerifyCommit_BrokenChain_ReturnsErrChainBroken(t *testing.T)
func TestRollbackRequest_ConflictingChanges_ReturnsErrRollbackConflict(t *testing.T)
```

### File Names

- One concept per file
- Descriptive, lowercase, max 30 chars
- Plural for package-level functions, singular for methods

```
store/git/
  ├── repository.go       # Repository wrapper type and methods
  ├── commit.go           # CommitStore type and commit operations
  ├── branch.go           # BranchStore type and branch operations
  ├── diff.go             # DiffStore and diff utilities
  ├── object.go           # Low-level object helpers
  ├── tree.go             # Tree building, traversal
  └── worktree.go         # Worktree operations

service/
  ├── commit.go           # CommitService and create logic
  ├── squash.go           # SquashService and squash logic
  ├── rollback.go         # RollbackService and rollback logic
  ├── branch.go           # BranchService
  ├── diff.go             # DiffService
  ├── verify.go           # VerifyService
  ├── audit.go            # AuditService
  └── config.go           # ConfigService
```

---

## 3. Go Patterns Specific to mgit

### 3.1 go-git Repository Wrapper

mgit wraps go-git's low-level plumbing with a type-safe, context-aware interface. Never expose `*git.Repository` directly.

```go
// ✓ Good: Wrapped Repository with context propagation
type Repository struct {
    gitRepo *git.Repository
    storage storer.Storer
    mu      sync.RWMutex
    clock   func() time.Time // Injected clock
}

func OpenRepository(ctx context.Context, path string, clock func() time.Time) (*Repository, error) {
    gitRepo, err := git.PlainOpen(path)
    if err != nil {
        return nil, fmt.Errorf("open repository: %w", err)
    }
    return &Repository{
        gitRepo: gitRepo,
        storage: gitRepo.Storer,
        clock:   clock,
    }, nil
}

// Method: All Repository methods accept context.Context
func (r *Repository) GetCommit(ctx context.Context, commitID string) (*model.Commit, error) {
    // Never create new context; propagate incoming ctx
    commitHash := plumbing.NewHash(commitID)
    commit, err := r.gitRepo.CommitObject(commitHash)
    if err != nil {
        return nil, fmt.Errorf("get commit %s: %w", commitID, err)
    }
    return commitToModel(commit), nil
}

// ✗ Bad: Exposing go-git internals
type Repository struct {
    Repo *git.Repository // Leaked abstraction!
}

func (r *Repository) GetCommit(commitID string) (*git.Commit, error) {
    // Lost context propagation, no error wrapping
    return r.Repo.CommitObject(plumbing.NewHash(commitID))
}
```

### 3.2 Commit Creation Flow

Always follow this strict order:

1. Build tree from file contents
2. Create commit object (with metadata)
3. Update ref (branch HEAD)
4. Update SQLite task-commit index

```go
// ✓ Good: Ordered, atomic operations
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    // Step 1: Build tree
    treeID, err := s.gitStore.BuildTree(ctx, req.Changes)
    if err != nil {
        return nil, fmt.Errorf("build tree: %w", err)
    }

    // Step 2: Create commit object
    commitID, err := s.gitStore.CreateCommitObject(ctx, &CommitObjectInput{
        TreeID:     treeID,
        Parent:     req.ParentID,
        Message:    req.Message,
        AuthorID:   req.AgentID,
        Metadata:   req.Metadata,
        Timestamp:  s.clock(),
    })
    if err != nil {
        return nil, fmt.Errorf("create commit object: %w", err)
    }

    // Step 3: Update ref
    branchName := req.BranchName
    err = s.gitStore.UpdateRef(ctx, branchName, commitID)
    if err != nil {
        return nil, fmt.Errorf("update ref %s: %w", branchName, err)
    }

    // Step 4: Update SQLite index (append-only)
    err = s.indexStore.RecordTaskCommit(ctx, req.TaskID, commitID, s.clock())
    if err != nil {
        return nil, fmt.Errorf("record task-commit mapping: %w", err)
    }

    return &model.Commit{
        ID:         commitID,
        TaskID:     req.TaskID,
        Timestamp:  s.clock(),
        Message:    req.Message,
    }, nil
}

// ✗ Bad: Out-of-order, no error wrapping, skips index
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    commitID, _ := s.gitStore.CreateCommit(req.Changes, req.Message) // Skips tree building!
    s.gitStore.UpdateRef(req.BranchName, commitID)                   // No error handling
    // Forgot to update SQLite index!
    return &model.Commit{ID: commitID}, nil
}
```

### 3.3 Plumbing vs. Porcelain

Prefer **plumbing** (lower-level, more control) over **porcelain** (high-level, less control). mgit requires determinism.

```go
// ✓ Good: Plumbing API (deterministic, verifiable)
func (r *Repository) GetCommit(ctx context.Context, commitID string) (*model.Commit, error) {
    hash := plumbing.NewHash(commitID)
    obj, err := r.storage.EncodedObject(plumbing.CommitObject, hash)
    if err != nil {
        return nil, fmt.Errorf("get commit object: %w", err)
    }

    decoder := object.NewDecoder(obj)
    commit := &object.Commit{}
    if err := decoder.Decode(commit); err != nil {
        return nil, fmt.Errorf("decode commit: %w", err)
    }

    return commitToModel(commit), nil
}

// ✗ Bad: Porcelain API (hides implementation, harder to audit)
func (r *Repository) GetCommit(ctx context.Context, commitID string) (*model.Commit, error) {
    commit, err := r.gitRepo.CommitObject(plumbing.NewHash(commitID))
    // No insight into decoding, storage, caching
    return commitToModel(commit), err
}
```

### 3.4 Clock Injection

Time is a dependency. Never call `time.Now()` directly. Inject a clock function into all services.

```go
// ✓ Good: Injected clock (testable, deterministic)
type CommitService struct {
    gitStore  GitStore
    indexStore IndexStore
    clock      func() time.Time // Injected
}

func NewCommitService(gitStore GitStore, indexStore IndexStore, clock func() time.Time) *CommitService {
    return &CommitService{
        gitStore:   gitStore,
        indexStore: indexStore,
        clock:      clock, // Use this, not time.Now()
    }
}

func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    timestamp := s.clock() // Deterministic in tests
    // ... create commit with timestamp
}

// In main.go: Inject real clock
svc := NewCommitService(gitStore, indexStore, time.Now)

// In tests: Inject fake clock
fakeTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
svc := NewCommitService(gitStore, indexStore, func() time.Time { return fakeTime })

// ✗ Bad: Direct time.Now() call (untestable, non-deterministic)
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    timestamp := time.Now() // Non-deterministic! Can't test!
    // ...
}
```

### 3.5 Context Propagation

All service methods accept `context.Context` as the first parameter. Propagate context through all layers.

```go
// ✓ Good: Context as first param, propagated through layers
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    // Propagate to git store
    treeID, err := s.gitStore.BuildTree(ctx, req.Changes)
    if err != nil {
        return nil, fmt.Errorf("build tree: %w", err)
    }

    // Propagate to index store
    err = s.indexStore.RecordTaskCommit(ctx, req.TaskID, commitID, s.clock())
    if err != nil {
        return nil, fmt.Errorf("record task-commit: %w", err)
    }

    return commit, nil
}

// ✗ Bad: Context ignored, leaked goroutines on cancellation
func (s *CommitService) CreateCommit(req *CommitRequest) (*model.Commit, error) {
    treeID, _ := s.gitStore.BuildTree(req.Changes) // No context!
    // If caller cancels, this keeps running
    go func() {
        s.indexStore.RecordTaskCommit(req.TaskID, commitID) // Ignores cancellation
    }()
    return commit, nil
}
```

---

## 4. Task-to-Commit Mapping Patterns

The SQLite index maintains a bidirectional mapping between tasks and commits, enabling efficient queries without git object scanning.

### 4.1 Schema

```sql
CREATE TABLE task_commits (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id             TEXT NOT NULL,        -- "PROJ-4.2.1"
    commit_id           TEXT NOT NULL,        -- SHA-256
    committed_at        DATETIME NOT NULL,    -- UTC
    created_at          DATETIME DEFAULT CURRENT_TIMESTAMP,
    agent_id            TEXT,                 -- Which agent created
    model_version       TEXT,                 -- Claude 3.5 Sonnet
    token_count         INTEGER,              -- Tokens used
    UNIQUE(task_id, commit_id),
    INDEX idx_task_id (task_id),
    INDEX idx_commit_id (commit_id),
    INDEX idx_task_committed (task_id, committed_at DESC)
);

CREATE TABLE branches (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    commit_id           TEXT NOT NULL,
    is_head             BOOLEAN DEFAULT 0,
    created_at          DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at          DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### 4.2 Append-Only Operations

The `task_commits` table is **append-only**. Inserts only; never DELETE or UPDATE.

```go
// ✓ Good: Append-only insertion
func (s *IndexStore) RecordTaskCommit(ctx context.Context, taskID model.TaskID, commitID string, timestamp time.Time) error {
    query := `
        INSERT INTO task_commits (task_id, commit_id, committed_at, created_at)
        VALUES (?, ?, ?, ?)
    `
    stmt, err := s.writeDB.PrepareContext(ctx, query)
    if err != nil {
        return fmt.Errorf("prepare insert: %w", err)
    }
    defer stmt.Close()

    _, err = stmt.ExecContext(ctx, taskID.Value, commitID, timestamp, time.Now().UTC())
    if err != nil {
        return fmt.Errorf("insert task-commit %s->%s: %w", taskID.Value, commitID, err)
    }
    return nil
}

// ✗ Bad: Attempting to delete or update (FORBIDDEN)
func (s *IndexStore) RemoveTaskCommit(taskID model.TaskID, commitID string) error {
    // FORBIDDEN! Breaks immutability and audit trail!
    _, err := s.writeDB.Exec(`
        DELETE FROM task_commits WHERE task_id = ? AND commit_id = ?
    `, taskID.Value, commitID)
    return err
}
```

### 4.3 Bidirectional Queries

Query from task → commits and commit → task with proper indexing.

```go
// ✓ Good: Indexed queries, parameterized
func (s *IndexStore) GetTaskCommits(ctx context.Context, taskID model.TaskID) ([]string, error) {
    query := `
        SELECT commit_id FROM task_commits
        WHERE task_id = ?
        ORDER BY committed_at DESC
    `
    rows, err := s.readDB.QueryContext(ctx, query, taskID.Value)
    if err != nil {
        return nil, fmt.Errorf("query task commits: %w", err)
    }
    defer rows.Close()

    var commitIDs []string
    for rows.Next() {
        var commitID string
        if err := rows.Scan(&commitID); err != nil {
            return nil, fmt.Errorf("scan commit id: %w", err)
        }
        commitIDs = append(commitIDs, commitID)
    }
    return commitIDs, rows.Err()
}

func (s *IndexStore) GetCommitTask(ctx context.Context, commitID string) (model.TaskID, error) {
    query := `SELECT task_id FROM task_commits WHERE commit_id = ? LIMIT 1`
    var taskID string
    err := s.readDB.QueryRowContext(ctx, query, commitID).Scan(&taskID)
    if err == sql.ErrNoRows {
        return model.TaskID{}, ErrCommitNotFound
    }
    if err != nil {
        return model.TaskID{}, fmt.Errorf("get commit task: %w", err)
    }
    return model.TaskID{Value: taskID}, nil
}

// ✗ Bad: String interpolation (SQL injection), no parameterization
func (s *IndexStore) GetTaskCommits(taskID model.TaskID) []string {
    query := fmt.Sprintf(`SELECT commit_id FROM task_commits WHERE task_id = '%s'`, taskID.Value)
    // SQL INJECTION VULNERABILITY!
    rows, _ := s.readDB.Query(query)
    // ...
}
```

### 4.4 Subtree Queries with LIKE Pattern

Efficiently query commit subtrees using hierarchical task IDs.

```go
// ✓ Good: LIKE pattern for subtree queries
func (s *IndexStore) GetSubtreeCommits(ctx context.Context, taskID model.TaskID) ([]string, error) {
    // Query: taskID='PROJ-4.2' finds 'PROJ-4.2.1', 'PROJ-4.2.2', etc.
    query := `
        SELECT DISTINCT commit_id FROM task_commits
        WHERE task_id LIKE ? || '%'
        ORDER BY committed_at DESC
    `
    rows, err := s.readDB.QueryContext(ctx, query, taskID.Value)
    if err != nil {
        return nil, fmt.Errorf("query subtree commits: %w", err)
    }
    defer rows.Close()

    var commitIDs []string
    for rows.Next() {
        var commitID string
        if err := rows.Scan(&commitID); err != nil {
            return nil, fmt.Errorf("scan: %w", err)
        }
        commitIDs = append(commitIDs, commitID)
    }
    return commitIDs, rows.Err()
}

// ✗ Bad: Post-query filtering (inefficient, unbounded memory)
func (s *IndexStore) GetSubtreeCommits(taskID model.TaskID) []string {
    rows, _ := s.readDB.Query(`SELECT commit_id FROM task_commits`)
    // Fetch entire table, then filter in Go
    var commitIDs []string
    for rows.Next() {
        var commitID, tid string
        rows.Scan(&tid, &commitID)
        if strings.HasPrefix(tid, taskID.Value) { // Post-filter!
            commitIDs = append(commitIDs, commitID)
        }
    }
    return commitIDs
}
```

### 4.5 Dual Connection Pools

SQLite requires careful concurrency management. Use separate read and write pools.

```go
// ✓ Good: Dual connection pools
type IndexStore struct {
    readDB  *sql.DB
    writeDB *sql.DB
}

func OpenIndexStore(ctx context.Context, dbPath string) (*IndexStore, error) {
    // Read pool: concurrent reads allowed
    readDB, err := sql.Open("sqlite", dbPath)
    if err != nil {
        return nil, fmt.Errorf("open read db: %w", err)
    }
    readDB.SetMaxOpenConns(25)
    readDB.SetMaxIdleConns(5)

    // Write pool: single writer (serialized)
    writeDB, err := sql.Open("sqlite", dbPath)
    if err != nil {
        return nil, fmt.Errorf("open write db: %w", err)
    }
    writeDB.SetMaxOpenConns(1) // Single writer!
    writeDB.SetMaxIdleConns(0)

    // Verify connectivity
    if err := readDB.PingContext(ctx); err != nil {
        return nil, fmt.Errorf("ping read db: %w", err)
    }
    if err := writeDB.PingContext(ctx); err != nil {
        return nil, fmt.Errorf("ping write db: %w", err)
    }

    return &IndexStore{readDB: readDB, writeDB: writeDB}, nil
}

// ✗ Bad: Single connection pool (serializes reads, lock contention)
type IndexStore struct {
    db *sql.DB
}

// All reads and writes block each other!
func (s *IndexStore) GetTaskCommits(taskID model.TaskID) []string {
    rows, _ := s.db.Query(...) // Blocks writes!
}
```

---

## 5. Error Handling

Errors are structured, wrapped with context, and traced through the call stack.

### 5.1 Wrapping with Context

Always wrap errors with `fmt.Errorf` using `%w` to preserve the error chain.

```go
// ✓ Good: Wrapped with context
func (s *SquashService) SquashTask(ctx context.Context, taskID model.TaskID) (string, error) {
    commits, err := s.indexStore.GetTaskCommits(ctx, taskID)
    if err != nil {
        return "", fmt.Errorf("squash task %s: get commits: %w", taskID.Value, err)
    }

    commitObj, err := s.gitStore.GetCommit(ctx, commits[0])
    if err != nil {
        return "", fmt.Errorf("squash task %s: get commit %s: %w", taskID.Value, commits[0], err)
    }

    resultID, err := s.gitStore.SquashCommits(ctx, commits)
    if err != nil {
        return "", fmt.Errorf("squash task %s: squash commits: %w", taskID.Value, err)
    }

    return resultID, nil
}

// ✗ Bad: Swallowed errors, no context
func (s *SquashService) SquashTask(taskID model.TaskID) (string, error) {
    commits, _ := s.indexStore.GetTaskCommits(taskID) // Error swallowed!
    if commits == nil {
        return "", errors.New("squash failed") // Lost context!
    }
    // ...
}
```

### 5.2 Sentinel Errors

Define errors in `model/errors.go`, export as variables (not constants), check with `errors.Is()`.

```go
// model/errors.go
var (
    ErrCommitNotFound     = errors.New("commit not found")
    ErrTaskNotFound       = errors.New("task not found")
    ErrBranchNotFound     = errors.New("branch not found")
    ErrInvalidTaskID      = errors.New("invalid task ID format")
)

// Usage in handler
func (h *CommitHandler) GetCommit(w http.ResponseWriter, r *http.Request) {
    commit, err := h.svc.GetCommit(r.Context(), commitID)
    if errors.Is(err, model.ErrCommitNotFound) {
        http.Error(w, "Commit not found", http.StatusNotFound)
        return
    }
    if err != nil {
        http.Error(w, "Internal error", http.StatusInternalServerError)
        h.log.Error("get commit", zap.Error(err))
        return
    }
    // ... return commit
}

// ✗ Bad: String comparison (fragile, loses type)
if err.Error() == "commit not found" { // WRONG!
    // ...
}
```

### 5.3 go-git Error Wrapping

Wrap go-git errors into mgit domain errors.

```go
// ✓ Good: Wrap go-git errors into mgit errors
func (r *Repository) GetCommit(ctx context.Context, commitID string) (*model.Commit, error) {
    hash := plumbing.NewHash(commitID)
    commit, err := r.gitRepo.CommitObject(hash)
    if err == plumbing.ErrObjectNotFound {
        return nil, fmt.Errorf("get commit: %w", model.ErrCommitNotFound)
    }
    if err != nil {
        return nil, fmt.Errorf("get commit %s: %w", commitID, err)
    }
    return commitToModel(commit), nil
}

// ✗ Bad: Expose go-git errors directly
func (r *Repository) GetCommit(commitID string) (*model.Commit, error) {
    hash := plumbing.NewHash(commitID)
    commit, err := r.gitRepo.CommitObject(hash)
    return commitToModel(commit), err // Leaks go-git errors!
}
```

### 5.4 Never Swallow Errors

Every error must be handled or wrapped.

```go
// ✓ Good: All errors handled or wrapped
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    if err := validateRequest(req); err != nil {
        return nil, fmt.Errorf("validate request: %w", err)
    }

    treeID, err := s.gitStore.BuildTree(ctx, req.Changes)
    if err != nil {
        return nil, fmt.Errorf("build tree: %w", err)
    }

    commitID, err := s.gitStore.CreateCommitObject(ctx, ...)
    if err != nil {
        return nil, fmt.Errorf("create commit: %w", err)
    }

    // ... continue with all errors handled
    return commit, nil
}

// ✗ Bad: Swallowed errors
func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    validateRequest(req)                           // Error ignored!
    treeID, _ := s.gitStore.BuildTree(ctx, ...)  // Error ignored!
    commitID, _ := s.gitStore.CreateCommitObject(...) // Error ignored!
    return commit, nil
}
```

---

## 6. Architecture Constraints

Strict layering enforces isolation and testability. No circular dependencies.

### 6.1 Dependency Rules

```
cmd/ (depends on service/)
    ↓
service/ (depends on store/ and model/)
    ↓
store/ (depends on model/)
    ↓
model/ (depends on nothing)
```

### 6.2 Package Boundaries

- **model/**: Pure types, no I/O, no side effects
- **store/**: Repository abstractions, wrapped by Interfaces defined in service/
- **service/**: Business logic, orchestrates store/ and model/
- **api/**: HTTP handlers, calls service/
- **cmd/**: CLI glue, calls service/

### 6.3 Interface Definition

Interfaces belong in service/, implemented in store/. Services define contracts, stores implement them.

```go
// service/service.go: Interface definition
type CommitStore interface {
    BuildTree(ctx context.Context, changes []*model.FileDiff) (string, error)
    CreateCommitObject(ctx context.Context, in *CommitObjectInput) (string, error)
    UpdateRef(ctx context.Context, branchName, commitID string) error
    GetCommit(ctx context.Context, commitID string) (*model.Commit, error)
}

type IndexStore interface {
    RecordTaskCommit(ctx context.Context, taskID model.TaskID, commitID string, ts time.Time) error
    GetTaskCommits(ctx context.Context, taskID model.TaskID) ([]string, error)
    GetCommitTask(ctx context.Context, commitID string) (model.TaskID, error)
}

type CommitService struct {
    gitStore   CommitStore
    indexStore IndexStore
    clock      func() time.Time
}

// store/git/commit.go: Implementation
type CommitStoreImpl struct {
    repo *Repository
}

func (c *CommitStoreImpl) BuildTree(ctx context.Context, changes []*model.FileDiff) (string, error) {
    // ... implementation
}

// ✗ Bad: Interface in store/, forces circular import
// store/git/service.go
type RepositoryService interface { ... }

// service/commit.go (imports store/git)
func (s *CommitService) foo(ctx context.Context) error {
    // Circular dependency!
}
```

### 6.4 No Circular Imports

Circular imports prevent compilation. Use dependency injection.

```go
// ✓ Good: Service depends on store interfaces
// service/commit.go
type CommitService struct {
    store CommitStore // Interface, defined in service/
}

// store/git/store.go
type Store struct {
    // Implements CommitStore
}

// ✗ Bad: Circular import
// service/commit.go
import "mgit/internal/store/git"

type CommitService struct {
    store *git.Store // Direct dependency
}

// store/git/store.go
import "mgit/internal/service"

func (s *Store) CreateCommit(...) error {
    // Calls service layer -> circular!
    s.svc.Audit(...)
}
```

---

## 7. Forbidden Patterns

These patterns are prohibited. Violations are code review rejections.

### 7.1 exec.Command("git", ...)

Never shell out to git. Use go-git exclusively. Shells are unmaintainable and non-deterministic.

```go
// ✗ FORBIDDEN: Shell invocation
func (r *Repository) CreateCommit(changes map[string]string) error {
    cmd := exec.Command("git", "commit", "-m", "...")
    return cmd.Run()
}

// ✓ Good: Pure Go with go-git
func (r *Repository) CreateCommit(ctx context.Context, changes map[string]string) (string, error) {
    treeID, _ := r.buildTree(ctx, changes)
    commitID, _ := r.createCommitObject(ctx, treeID, ...)
    return commitID, nil
}
```

### 7.2 DELETE FROM task_commits

The task-commit index is **append-only**. No deletes, no updates.

```go
// ✗ FORBIDDEN: Deleting from task_commits breaks audit trail
func (s *IndexStore) RemoveTaskCommit(taskID model.TaskID, commitID string) error {
    _, err := s.db.Exec(`DELETE FROM task_commits WHERE task_id = ? AND commit_id = ?`, ...)
    return err
}

// ✓ Good: If correction needed, INSERT a new record with updated metadata
func (s *IndexStore) MarkTaskCommitInvalid(ctx context.Context, taskID model.TaskID, commitID string) error {
    // Append a marker record instead of deleting
    query := `INSERT INTO task_commits (task_id, commit_id, committed_at, created_at, metadata)
              VALUES (?, ?, ?, ?, ?)`
    _, err := s.writeDB.ExecContext(ctx, query, taskID.Value, commitID, time.Now(), time.Now(),
        `{"status": "invalid", "reason": "..."}`)
    return err
}
```

### 7.3 time.Now()

Never call `time.Now()` directly. Inject the clock.

```go
// ✗ FORBIDDEN: Direct time.Now()
func (s *CommitService) CreateCommit(req *CommitRequest) (*model.Commit, error) {
    commit := &model.Commit{
        Timestamp: time.Now(), // Non-deterministic in tests!
    }
    return commit, nil
}

// ✓ Good: Use injected clock
func (s *CommitService) CreateCommit(req *CommitRequest) (*model.Commit, error) {
    commit := &model.Commit{
        Timestamp: s.clock(), // Testable
    }
    return commit, nil
}
```

### 7.4 String Interpolation in SQL

Never concatenate user input into SQL. Always use parameterized queries.

```go
// ✗ FORBIDDEN: SQL injection vulnerability
func (s *IndexStore) GetTaskCommits(taskID model.TaskID) ([]string, error) {
    query := fmt.Sprintf(`SELECT commit_id FROM task_commits WHERE task_id = '%s'`, taskID.Value)
    rows, err := s.db.Query(query) // Injection point!
    // ...
}

// ✓ Good: Parameterized queries
func (s *IndexStore) GetTaskCommits(ctx context.Context, taskID model.TaskID) ([]string, error) {
    query := `SELECT commit_id FROM task_commits WHERE task_id = ?`
    rows, err := s.db.QueryContext(ctx, query, taskID.Value) // Safe!
    // ...
}
```

### 7.5 Hardcoded Paths

Never hardcode filesystem paths. Use config or auto-detection.

```go
// ✗ FORBIDDEN: Hardcoded paths
func OpenRepository() (*Repository, error) {
    return git.PlainOpen("/home/user/.mgit/repo") // Won't work on Windows!
}

// ✓ Good: Config-based or auto-detected
type Config struct {
    RepoPath string
}

func (c *Config) DefaultRepoPath() string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".mgit", "repo")
}

func OpenRepository(ctx context.Context, cfg *Config) (*Repository, error) {
    path := cfg.RepoPath
    if path == "" {
        path = cfg.DefaultRepoPath()
    }
    return git.PlainOpen(path)
}
```

### 7.6 In-Memory Test Databases

Never use in-memory SQLite (`:memory:`) in tests. Use `t.TempDir()`.

```go
// ✗ FORBIDDEN: In-memory test database (not durable, debugging impossible)
func TestIndexStore(t *testing.T) {
    db, _ := sql.Open("sqlite", ":memory:")
    store := &IndexStore{db: db}
    // Test... but if it fails, no trace to examine
}

// ✓ Good: Temporary directory (durable, inspectable)
func TestIndexStore(t *testing.T) {
    tmpDir := t.TempDir()
    dbPath := filepath.Join(tmpDir, "test.db")

    db, err := sql.Open("sqlite", "file:" + dbPath)
    if err != nil {
        t.Fatalf("open db: %v", err)
    }

    store := &IndexStore{readDB: db, writeDB: db}
    // Test...
    // After test fails, examine /tmp/TestIndexStore.xxx/test.db with sqlite3 CLI
}
```

---

## 8. Safety and Compliance

mgit is safety-critical. Code must meet DO-178C, IEC 62304, NASA-STD-8739.8, MIL-STD-498, OWASP ASVS Level 2.

### 8.1 Determinism

- No randomness except PRNG in tests
- Clock injected; no `time.Now()`
- All operations traceable and auditable
- Commit hashes derived from content (no random IDs)

### 8.2 Audit Trail

- All state changes logged to SQLite (append-only)
- No silent failures; errors wrapped with context
- Task-commit mappings permanent; deletions forbidden

### 8.3 Immutability

- Commit history immutable; no rewrites
- Branches can be deleted/recreated, but commit chain persists
- Task-commit index append-only

### 8.4 Testing

- Unit tests with injected mocks
- Integration tests with temporary databases
- E2E tests with real git repos
- 80%+ coverage minimum

---

## 9. Code Review Checklist

Every PR must satisfy:

- [ ] All package dependencies respect layering rules
- [ ] No `time.Now()` calls; clock injected
- [ ] All errors wrapped with `fmt.Errorf(..., %w, ...)`
- [ ] Context propagated as first parameter
- [ ] No `exec.Command("git", ...)`
- [ ] SQLite queries parameterized (no string interpolation)
- [ ] test_commits table append-only (no DELETE/UPDATE)
- [ ] File ≤ 500 lines; split if longer
- [ ] Test names follow `Test{Function}_{Scenario}_{Expected}` pattern
- [ ] Service interfaces defined in service/, not store/
- [ ] No circular imports
- [ ] Hardcoded paths removed (use config)
- [ ] In-memory test databases replaced with t.TempDir()
- [ ] 80%+ test coverage on modified code
- [ ] Error handling for every error return

---

## 10. Dual-Hash Model (ADR-002)

mgit uses two hashing algorithms — this is intentional and mandatory. See ADR-002 for full rationale.

### Git Object IDs (SHA-1) — go-git native
```go
// go-git produces SHA-1 hashes for git objects. This is unavoidable.
commitHash, err := worktree.Commit(message, &git.CommitOptions{Author: sig})
// commitHash is a plumbing.Hash (SHA-1, 20 bytes)
```

### mgit Content Hash (SHA-256) — application layer
```go
// mgit computes its own SHA-256 integrity hash for every commit.
// This is the authoritative hash for audit/compliance purposes.
func computeContentHash(msg, fileDiffsJSON, taskID, parentContentHash, createdAt string) string {
    h := sha256.New()
    h.Write([]byte(msg))
    h.Write([]byte(fileDiffsJSON))
    h.Write([]byte(taskID))
    h.Write([]byte(parentContentHash))
    h.Write([]byte(createdAt))
    return hex.EncodeToString(h.Sum(nil))
}
```

### Both hashes stored together
```go
type Commit struct {
    CommitID    string `json:"commit_id"`     // SHA-1 from go-git
    ContentHash string `json:"content_hash"`  // SHA-256 from mgit
    // ...
}
```

### Verification checks both
```go
// mgit verify checks:
// 1. SHA-1: recompute from git object → must match CommitID
// 2. SHA-256: recompute from metadata → must match ContentHash
// If SHA-1 matches but SHA-256 doesn't → mgit metadata tampered
// If SHA-1 doesn't match → git object tampered
// Both must match for "verified" status
```

### Rules
- NEVER use SHA-1 for security decisions — it is structural only
- ALWAYS use ContentHash (SHA-256) for integrity assertions in audit trail
- ALWAYS store both hashes in the task_commits SQLite index
- NEVER skip ContentHash computation — even for system/rollback commits

---

## 11. Fsync-Aware Storage (NFR-3.3a)

go-git's default filesystem storage does not guarantee fsync. mgit wraps the storer:

```go
// ✓ CORRECT — wrap go-git storer with sync guarantee
type SyncingStorer struct {
    storer storage.Storer
}

func (s *SyncingStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
    hash, err := s.storer.SetEncodedObject(obj)
    if err != nil {
        return plumbing.ZeroHash, err
    }
    // fsync the object file
    if err := s.syncObjectFile(hash); err != nil {
        return plumbing.ZeroHash, fmt.Errorf("fsync object: %w", err)
    }
    return hash, nil
}

// ✗ FORBIDDEN — using go-git storer directly without fsync wrapper
repo.Storer.SetEncodedObject(obj) // NO! Wrap with SyncingStorer
```

---

## 12. References

- **go-git**: https://github.com/go-git/go-git
- **modernc.org/sqlite**: https://pkg.go.dev/modernc.org/sqlite
- **Cobra**: https://cobra.dev
- **Echo**: https://echo.labstack.com
- **DO-178C**: Avionics software certification standard
- **IEC 62304**: Medical device software lifecycle
- **OWASP ASVS**: Application security verification

---

**Last Updated**: 2024-Q1
**Version**: 1.0
**Status**: Active (enforced in code review)
