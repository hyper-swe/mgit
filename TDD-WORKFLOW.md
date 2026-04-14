# mgit TDD Workflow

**Safety-Critical Micro Git Version Control for LLM Coding Agents**

This document defines the exact Test-Driven Development (TDD) workflow for mgit. TDD is **mandatory, not optional** in this codebase.

---

## 1. TDD Philosophy

mgit is a **safety-critical system** used in NASA, airline, hospital, and DoD environments. In these contexts:

- **"If it's not tested, it doesn't work."**
- Tests are not optional QA additions—they are **executable specifications**.
- Tests define behavior *before* implementation.
- Every feature must have comprehensive tests that pass/fail correctly.
- Coverage must exceed 90% to ensure safety.

### Why TDD for mgit?

1. **Append-only immutability** must be strictly enforced—tests verify no destructive operations
2. **Commit chain integrity** is critical—tests verify parent-child hashes
3. **Atomic squash operations** must never partially succeed—tests verify all-or-nothing
4. **Rollback correctness** demands idempotence—tests verify consistent state
5. **go-git integration** requires deterministic behavior—tests verify real git operations
6. **Concurrency safety** is essential—tests verify race-free implementations

**TDD is how we guarantee correctness in a safety-critical system.**

---

## 2. The TDD Cycle

The following 8-step sequence must be followed with **zero deviations**:

```
Step 1:  READ the requirement (FR/NFR from REQUIREMENTS.md)
Step 2:  WRITE failing test(s) — the test defines the behavior
Step 3:  RUN test → CONFIRM IT FAILS (red)
Step 4:  WRITE the minimum production code to make the test pass
Step 5:  RUN test → CONFIRM IT PASSES (green)
Step 6:  REFACTOR — clean up while keeping all tests green
Step 7:  CHECK coverage ≥ 90%
Step 8:  COMMIT — test and production code together
```

### Critical Rule: **You MUST verify the test fails before writing production code.**

If the test passes immediately, it's either testing existing behavior or the test is trivial. Either way, **stop and rewrite the test** to ensure it actually validates new behavior.

Example: If you write `TestCreateCommit_ValidRequest_CreatesCommitObject` and it passes before you implement `CreateCommit`, your test is broken. Rewrite it to be more specific, or it doesn't prove anything.

---

## 3. Worked Example: CommitService.CreateCommit

This example walks through a complete TDD cycle for implementing `CommitService.CreateCommit`.

### Step 1: READ the Requirement

- **FR-2.1**: Task-tagged commits (commits must carry a TaskID)
- **FR-3**: Commit data model (Hash, ParentHash, TaskID, Timestamp, Message, FileDiffs)
- **FR-4.1**: Task-commit mapping (maintain SQLite index mapping TaskID → CommitHash)

### Step 2: WRITE Failing Test

```go
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {
    // Arrange: set up mock git store, mock index store, fixed clock
    clock := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
    gitStore := &mockGitStore{
        createTreeFunc: func(ctx context.Context, diffs []*model.FileDiff) (string, error) {
            return "abc123def456", nil
        },
        createCommitFunc: func(ctx context.Context, treeHash, parentHash, message string, timestamp time.Time) (string, error) {
            return "commit789abc123", nil
        },
        updateRefFunc: func(ctx context.Context, refName, commitHash string) error {
            return nil
        },
    }

    indexStore := &mockIndexStore{
        recordFunc: func(ctx context.Context, taskID string, commitHash string) error {
            return nil
        },
    }

    svc := NewCommitService(gitStore, indexStore, clock)

    req := &CommitRequest{
        TaskID:  model.TaskID{Value: "PROJ-4.2.1"},
        Message: "implement validation",
        Changes: []*model.FileDiff{
            {Path: "main.go", Added: []string{"func Validate() {}"}},
        },
    }

    // Act
    commit, err := svc.CreateCommit(context.Background(), req)

    // Assert
    require.NoError(t, err)
    assert.Equal(t, "PROJ-4.2.1", commit.TaskID.Value)
    assert.Equal(t, "commit789abc123", commit.Hash)
    assert.Equal(t, "implement validation", commit.Message)
    assert.True(t, indexStore.RecordCalled)
    assert.Equal(t, "PROJ-4.2.1", indexStore.RecordedTaskID)
    assert.Equal(t, "commit789abc123", indexStore.RecordedCommitHash)
}
```

### Step 3: RUN the Test (Expect FAILURE)

```bash
$ go test ./internal/service/ -run TestCreateCommit_WithValidTaskID_StoresMapping -v
--- FAIL: TestCreateCommit_WithValidTaskID_StoresMapping (0.01s)
    service_test.go:45: undefined: CommitService
    service_test.go:46: undefined: NewCommitService
FAIL
```

**✓ Confirm the test fails.** If it passed, the implementation already existed—stop and refactor.

### Step 4: WRITE Minimum Production Code

```go
// internal/service/commit_service.go

type CommitService struct {
    gitStore   GitStore
    indexStore IndexStore
    clock      func() time.Time
}

func NewCommitService(gitStore GitStore, indexStore IndexStore, clock func() time.Time) *CommitService {
    return &CommitService{gitStore, indexStore, clock}
}

type CommitRequest struct {
    TaskID  model.TaskID
    Message string
    Changes []*model.FileDiff
}

func (s *CommitService) CreateCommit(ctx context.Context, req *CommitRequest) (*model.Commit, error) {
    // Step 1: Build tree from diffs
    treeHash, err := s.gitStore.CreateTree(ctx, req.Changes)
    if err != nil {
        return nil, err
    }

    // Step 2: Get parent commit (HEAD)
    parentHash, err := s.gitStore.GetHEAD(ctx)
    if err != nil && err != ErrNoHEAD {
        return nil, err
    }

    // Step 3: Create commit object in git
    commitHash, err := s.gitStore.CreateCommit(ctx, treeHash, parentHash, req.Message, s.clock())
    if err != nil {
        return nil, err
    }

    // Step 4: Update HEAD ref
    if err := s.gitStore.UpdateRef(ctx, "HEAD", commitHash); err != nil {
        return nil, err
    }

    // Step 5: Record task-commit mapping in index
    if err := s.indexStore.Record(ctx, req.TaskID.Value, commitHash); err != nil {
        return nil, err
    }

    // Step 6: Return commit object
    commit := &model.Commit{
        Hash:      commitHash,
        ParentHash: parentHash,
        TaskID:    req.TaskID,
        Message:   req.Message,
        Timestamp: s.clock(),
    }

    return commit, nil
}
```

### Step 5: RUN the Test (Expect PASS)

```bash
$ go test ./internal/service/ -run TestCreateCommit_WithValidTaskID_StoresMapping -v
--- PASS: TestCreateCommit_WithValidTaskID_StoresMapping (0.01s)
PASS
```

**✓ Confirm the test passes.** The implementation is correct for this scenario.

### Step 6: REFACTOR (Keep Tests Green)

No refactoring needed yet—the code is simple and clear. If you added helper methods or extracted logic, ensure all tests still pass.

### Step 7: CHECK Coverage

```bash
$ go test ./internal/service/ -coverprofile=cover.out -count=1
$ go tool cover -func=cover.out | grep CommitService
internal/service/commit_service.go   NewCommitService       100.0%
internal/service/commit_service.go   CreateCommit           100.0%
```

**✓ Confirm coverage ≥ 90%.** Continue adding error path tests.

### Step 8: COMMIT

```bash
$ git add internal/service/commit_service.go internal/service/commit_service_test.go
$ git commit -m "feat(service): implement CreateCommit with task-commit mapping

- Create tree from FileDiffs
- Create commit object with parent hash
- Update HEAD ref
- Record TaskID-CommitHash mapping in index
- Return Commit model with all metadata

Refs: MGIT-3.1.1, FR-2.1, FR-3, FR-4.1"
```

**✓ Commit test and production code together.**

---

## 4. Worked Example: SquashService.SquashTask

This example emphasizes **atomic operations** and **rollback on failure**.

### Step 1: READ the Requirement

- **FR-5.1**: Squash multiple commits into one consolidated commit
- **FR-5.2**: Atomic operation—all or nothing (if any step fails, rollback all changes)
- **FR-5.3**: Update task-commit index to point to new consolidated commit
- **FR-5.4**: Maintain commit chain integrity (new commit parent is original first commit's parent)

### Step 2: WRITE Failing Test (Happy Path)

```go
func TestSquashTask_MultipleCommits_ProducesSingleConsolidatedCommit(t *testing.T) {
    gitStore := setupMockGitStore()
    indexStore := setupMockIndexStore()
    svc := NewSquashService(gitStore, indexStore)

    taskID := "PROJ-4.2.1"
    commits := []*model.Commit{
        {Hash: "commit1", ParentHash: "parent", TaskID: model.TaskID{Value: taskID}},
        {Hash: "commit2", ParentHash: "commit1", TaskID: model.TaskID{Value: taskID}},
        {Hash: "commit3", ParentHash: "commit2", TaskID: model.TaskID{Value: taskID}},
    }

    // Act
    result, err := svc.SquashTask(context.Background(), taskID, commits)

    // Assert
    require.NoError(t, err)
    assert.Equal(t, 1, len(result.Commits))
    assert.Equal(t, "parent", result.Commits[0].ParentHash)
    assert.Equal(t, taskID, result.Commits[0].TaskID.Value)
    assert.NotEqual(t, "commit1", result.Commits[0].Hash) // New hash, not original
}
```

### Step 3: RUN the Test (Expect FAILURE)

```bash
$ go test ./internal/service/ -run TestSquashTask_MultipleCommits_ProducesSingleConsolidatedCommit -v
--- FAIL: TestSquashTask_MultipleCommits_ProducesSingleConsolidatedCommit (0.01s)
FAIL
```

### Step 2b: WRITE Failing Test (Atomicity—Partial Failure)

```go
func TestSquashTask_AtomicFailure_RollsBackAllChanges(t *testing.T) {
    gitStore := &mockGitStore{
        createCommitFunc: func(ctx context.Context, treeHash, parentHash, message string, timestamp time.Time) (string, error) {
            return "", errors.New("disk full")  // Simulate failure
        },
    }
    indexStore := &mockIndexStore{}
    svc := NewSquashService(gitStore, indexStore)

    taskID := "PROJ-4.2.1"
    commits := []*model.Commit{
        {Hash: "commit1", ParentHash: "parent", TaskID: model.TaskID{Value: taskID}},
        {Hash: "commit2", ParentHash: "commit1", TaskID: model.TaskID{Value: taskID}},
    }

    // Act
    result, err := svc.SquashTask(context.Background(), taskID, commits)

    // Assert
    require.Error(t, err)
    assert.Nil(t, result)
    assert.False(t, indexStore.RecordCalled)  // Verify no index update on failure
    // Verify git store was not modified (gitStore.UpdateRef should not have been called)
}
```

### Step 4: WRITE Minimum Production Code

```go
// internal/service/squash_service.go

type SquashService struct {
    gitStore   GitStore
    indexStore IndexStore
}

type SquashResult struct {
    Commits []*model.Commit
}

func (s *SquashService) SquashTask(ctx context.Context, taskID string, commits []*model.Commit) (*SquashResult, error) {
    if len(commits) == 0 {
        return nil, ErrNoCommitsToSquash
    }

    if len(commits) == 1 {
        return &SquashResult{Commits: commits}, nil
    }

    // Step 1: Gather all diffs from all commits
    var allDiffs []*model.FileDiff
    for _, c := range commits {
        diffs, err := s.gitStore.GetDiffs(ctx, c.Hash)
        if err != nil {
            return nil, err
        }
        allDiffs = append(allDiffs, diffs...)
    }

    // Step 2: Create single consolidated tree
    treeHash, err := s.gitStore.CreateTree(ctx, allDiffs)
    if err != nil {
        return nil, err  // Atomicity: fail before any permanent changes
    }

    // Step 3: Create single consolidated commit
    message := fmt.Sprintf("Squash %d commits for task %s", len(commits), taskID)
    parentHash := commits[0].ParentHash
    newCommitHash, err := s.gitStore.CreateCommit(ctx, treeHash, parentHash, message, time.Now())
    if err != nil {
        return nil, err  // Atomicity: fail before any permanent changes
    }

    // Step 4: Update HEAD (point to new commit)
    if err := s.gitStore.UpdateRef(ctx, "HEAD", newCommitHash); err != nil {
        // Atomicity: rollback—creation was a side effect, but no further changes needed
        return nil, err
    }

    // Step 5: Update index (map TaskID to new commit)
    if err := s.indexStore.Record(ctx, taskID, newCommitHash); err != nil {
        // Atomicity: rollback by resetting HEAD to original
        s.gitStore.UpdateRef(ctx, "HEAD", commits[len(commits)-1].Hash)
        return nil, err
    }

    // Step 6: Return success
    newCommit := &model.Commit{
        Hash:       newCommitHash,
        ParentHash: parentHash,
        TaskID:     model.TaskID{Value: taskID},
        Message:    message,
        Timestamp:  time.Now(),
    }

    return &SquashResult{Commits: []*model.Commit{newCommit}}, nil
}
```

### Step 5: RUN Tests (Expect PASS)

```bash
$ go test ./internal/service/ -run TestSquashTask -v
--- PASS: TestSquashTask_MultipleCommits_ProducesSingleConsolidatedCommit (0.01s)
--- PASS: TestSquashTask_AtomicFailure_RollsBackAllChanges (0.01s)
PASS
```

### Step 7: CHECK Coverage

```bash
$ go test ./internal/service/ -coverprofile=cover.out -count=1
$ go tool cover -func=cover.out | grep SquashService
internal/service/squash_service.go   SquashTask           95.0%
```

### Step 8: COMMIT

```bash
$ git commit -m "feat(service): implement SquashTask with atomic rollback

- Gather diffs from all commits
- Create consolidated tree
- Create single consolidated commit
- Update HEAD and index atomically
- Rollback on failure (all-or-nothing)

Refs: MGIT-5.1, MGIT-5.2, MGIT-5.3, MGIT-5.4"
```

---

## 5. Worked Example: RollbackService.Rollback

This example emphasizes **append-only enforcement** and **idempotence**.

### Step 1: READ the Requirement

- **FR-6.1**: Rollback a commit by creating a NEW revert commit (not deleting)
- **FR-6.2**: Original commit must remain in history (append-only)
- **FR-6.3**: Idempotence—rolling back the same commit twice produces identical result
- **FR-6.4**: Update task-commit index to point to revert commit

### Step 2: WRITE Failing Test (Happy Path)

```go
func TestRollback_AppendOnly_CreatesRevertCommit(t *testing.T) {
    gitStore := setupMockGitStore()
    indexStore := setupMockIndexStore()
    svc := NewRollbackService(gitStore, indexStore)

    originalCommit := &model.Commit{
        Hash:       "original123",
        ParentHash: "parent",
        TaskID:     model.TaskID{Value: "PROJ-4.2.1"},
        Message:    "add feature",
    }

    // Act
    revertCommit, err := svc.Rollback(context.Background(), originalCommit)

    // Assert
    require.NoError(t, err)
    assert.NotEqual(t, "original123", revertCommit.Hash)
    assert.Equal(t, "original123", revertCommit.ParentHash)  // Parent is the original commit
    assert.Contains(t, revertCommit.Message, "Revert")
    assert.Contains(t, revertCommit.Message, "original123")
    // Verify original commit is still in git history
    foundOriginal, _ := gitStore.GetCommit(context.Background(), "original123")
    assert.NotNil(t, foundOriginal)
}
```

### Step 2b: WRITE Failing Test (Idempotence)

```go
func TestRollback_Idempotent_SameResultOnSecondCall(t *testing.T) {
    gitStore := setupMockGitStore()
    indexStore := setupMockIndexStore()
    svc := NewRollbackService(gitStore, indexStore)

    commit := &model.Commit{
        Hash:       "commit123",
        ParentHash: "parent",
        TaskID:     model.TaskID{Value: "PROJ-4.2.1"},
    }

    // Act: Rollback first time
    revert1, err1 := svc.Rollback(context.Background(), commit)
    require.NoError(t, err1)

    // Act: Rollback again
    revert2, err2 := svc.Rollback(context.Background(), commit)
    require.NoError(t, err2)

    // Assert: Both reverts have the same content
    assert.Equal(t, revert1.Hash, revert2.Hash)
    assert.Equal(t, revert1.Message, revert2.Message)
}
```

### Step 4: WRITE Minimum Production Code

```go
// internal/service/rollback_service.go

type RollbackService struct {
    gitStore   GitStore
    indexStore IndexStore
}

func (s *RollbackService) Rollback(ctx context.Context, commit *model.Commit) (*model.Commit, error) {
    // Step 1: Verify commit exists (verify chain integrity)
    existing, err := s.gitStore.GetCommit(ctx, commit.Hash)
    if err != nil {
        return nil, ErrCommitNotFound
    }

    // Step 2: Check if already rolled back (idempotence)
    existingRevert, _ := s.indexStore.FindRevert(ctx, commit.Hash)
    if existingRevert != nil {
        return existingRevert, nil
    }

    // Step 3: Get inverse diffs (undo the changes)
    diffs, err := s.gitStore.GetDiffs(ctx, commit.Hash)
    if err != nil {
        return nil, err
    }
    inverseDiffs := invertDiffs(diffs)

    // Step 4: Create tree with inverse changes
    treeHash, err := s.gitStore.CreateTree(ctx, inverseDiffs)
    if err != nil {
        return nil, err
    }

    // Step 5: Create revert commit (parent is the original commit, not HEAD)
    message := fmt.Sprintf("Revert %s: %s", commit.Hash[:7], commit.Message)
    revertHash, err := s.gitStore.CreateCommit(ctx, treeHash, commit.Hash, message, time.Now())
    if err != nil {
        return nil, err
    }

    // Step 6: Record revert in index (for idempotence lookup)
    if err := s.indexStore.RecordRevert(ctx, commit.Hash, revertHash); err != nil {
        return nil, err
    }

    // Step 7: Return revert commit
    revertCommit := &model.Commit{
        Hash:       revertHash,
        ParentHash: commit.Hash,
        TaskID:     commit.TaskID,
        Message:    message,
        Timestamp:  time.Now(),
    }

    return revertCommit, nil
}

func invertDiffs(diffs []*model.FileDiff) []*model.FileDiff {
    inverted := make([]*model.FileDiff, len(diffs))
    for i, d := range diffs {
        inverted[i] = &model.FileDiff{
            Path:    d.Path,
            Added:   d.Removed,    // Swap
            Removed: d.Added,      // Swap
        }
    }
    return inverted
}
```

### Step 5: RUN Tests (Expect PASS)

```bash
$ go test ./internal/service/ -run TestRollback -v
--- PASS: TestRollback_AppendOnly_CreatesRevertCommit (0.01s)
--- PASS: TestRollback_Idempotent_SameResultOnSecondCall (0.01s)
PASS
```

### Step 8: COMMIT

```bash
$ git commit -m "feat(service): implement Rollback with append-only and idempotence

- Create revert commit (not delete)
- Original commit remains in history
- Parent of revert is original commit
- Idempotent—rolling back twice yields same hash
- Record revert mapping for idempotence

Refs: MGIT-6.1, MGIT-6.2, MGIT-6.3, MGIT-6.4"
```

---

## 6. Test Naming Convention

All test function names follow this strict pattern:

```
Test{FunctionUnderTest}_{Scenario}_{ExpectedResult}
```

Where:
- `{FunctionUnderTest}` = Name of the function being tested (e.g., `CreateCommit`, `SquashTask`)
- `{Scenario}` = The specific condition or context (e.g., `WithValidTaskID`, `MultipleCommits`, `AtomicFailure`)
- `{ExpectedResult}` = What should happen (e.g., `StoresMapping`, `ProducesSingleCommit`, `RollsBackAllChanges`)

### Examples

```go
// CommitService
func TestCreateCommit_ValidRequest_CreatesCommitObject(t *testing.T) {}
func TestCreateCommit_MissingTaskID_ReturnsErrInvalidTaskID(t *testing.T) {}
func TestCreateCommit_EmptyMessage_ReturnsErrMessageRequired(t *testing.T) {}
func TestCreateCommit_NoChanges_ReturnsErrNoChanges(t *testing.T) {}
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {}

// SquashService
func TestSquashTask_MultipleCommits_ProducesSingleCommit(t *testing.T) {}
func TestSquashTask_ZeroCommits_ReturnsErrNoCommitsToSquash(t *testing.T) {}
func TestSquashTask_OneCommit_ReturnsUnchanged(t *testing.T) {}
func TestSquashTask_AtomicFailure_RollsBackAllChanges(t *testing.T) {}
func TestSquashTask_DifferentTaskIDs_ReturnsErrMismatchedTasks(t *testing.T) {}

// RollbackService
func TestRollback_AppendOnly_CreatesRevertCommit(t *testing.T) {}
func TestRollback_Idempotent_SameResultOnSecondCall(t *testing.T) {}
func TestRollback_CommitNotFound_ReturnsErrCommitNotFound(t *testing.T) {}
func TestRollback_InvalidDiffs_ReturnsErrInvalidDiffs(t *testing.T) {}

// ChainVerification
func TestVerifyChain_BrokenLink_ReturnsErrChainBroken(t *testing.T) {}
func TestVerifyChain_ValidChain_ReturnsNil(t *testing.T) {}
func TestVerifyChain_CircularReference_ReturnsErrCircular(t *testing.T) {}
```

---

## 7. Table-Driven Tests

Mandatory for testing multiple scenarios. Always use table-driven tests when there are 2+ related test cases.

### Example: TaskID Validation

```go
func TestCreateCommit_VariousTaskIDFormats(t *testing.T) {
    tests := []struct {
        name      string
        taskID    string
        wantErr   error
    }{
        {"valid standard format", "PROJ-4.2.1", nil},
        {"valid single level", "PROJ-1", nil},
        {"valid max depth", "PROJ-1.2.3.4.5.6.7.8.9.10", nil},
        {"empty task ID", "", model.ErrInvalidTaskID},
        {"no project prefix", "4.2.1", model.ErrInvalidTaskID},
        {"invalid separator", "PROJ_4_2_1", model.ErrInvalidTaskID},
        {"exceeds max depth", "PROJ-" + strings.Repeat("1.", 51), model.ErrInvalidTaskID},
        {"non-numeric level", "PROJ-alpha.2.1", model.ErrInvalidTaskID},
        {"trailing dot", "PROJ-4.2.1.", model.ErrInvalidTaskID},
        {"leading dot", "PROJ-.4.2.1", model.ErrInvalidTaskID},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            svc := NewCommitService(setupMocks())
            _, err := svc.CreateCommit(context.Background(), &CommitRequest{
                TaskID:  model.TaskID{Value: tt.taskID},
                Message: "test",
                Changes: []*model.FileDiff{},
            })
            assert.Equal(t, tt.wantErr, err)
        })
    }
}
```

### Example: Boundary Testing

```go
func TestCreateCommit_FileSizeAndCount(t *testing.T) {
    tests := []struct {
        name     string
        fileSize int
        fileCount int
        wantErr  error
    }{
        {"single small file", 100, 1, nil},
        {"multiple files", 1024, 5, nil},
        {"max total size", 1024*1024*10, 1, nil},  // 10MB
        {"exceeds max size", 1024*1024*11, 1, model.ErrFileTooLarge},
        {"zero files", 0, 0, model.ErrNoChanges},
        {"max file count", 1024, 1000, nil},
        {"exceeds max file count", 1024, 1001, model.ErrTooManyFiles},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Create diffs matching size/count
            diffs := generateDiffs(tt.fileSize, tt.fileCount)
            _, err := testService.CreateCommit(context.Background(), &CommitRequest{
                TaskID:  model.TaskID{Value: "PROJ-1"},
                Message: "test",
                Changes: diffs,
            })
            assert.Equal(t, tt.wantErr, err)
        })
    }
}
```

---

## 8. Test Categories

Every feature must have tests covering all of these categories:

### 1. Happy Path Tests
Normal, successful operations with valid inputs.

```go
func TestCreateCommit_ValidRequest_CreatesCommitObject(t *testing.T) {}
func TestSquashTask_MultipleCommits_ProducesSingleCommit(t *testing.T) {}
func TestRollback_AppendOnly_CreatesRevertCommit(t *testing.T) {}
```

### 2. Error Path Tests
Every documented error condition must have a test. Check ERRORS.md for the complete list.

```go
func TestCreateCommit_MissingTaskID_ReturnsErrInvalidTaskID(t *testing.T) {}
func TestCreateCommit_EmptyMessage_ReturnsErrMessageRequired(t *testing.T) {}
func TestSquashTask_ZeroCommits_ReturnsErrNoCommitsToSquash(t *testing.T) {}
func TestSquashTask_DifferentTaskIDs_ReturnsErrMismatchedTasks(t *testing.T) {}
func TestRollback_CommitNotFound_ReturnsErrCommitNotFound(t *testing.T) {}
func TestVerifyChain_BrokenLink_ReturnsErrChainBroken(t *testing.T) {}
```

### 3. Boundary Tests
Empty input, maximum length, zero values, depth limits.

```go
func TestCreateCommit_EmptyChanges_ReturnsErrNoChanges(t *testing.T) {}
func TestCreateCommit_MaxDepthTaskID_Succeeds(t *testing.T) {}
func TestSquashTask_OneCommit_ReturnsUnchanged(t *testing.T) {}
func TestSquashTask_ManyCommits_Succeeds(t *testing.T) {}
```

### 4. Squash Atomicity Tests
Verify partial failure causes rollback, all-or-nothing semantics.

```go
func TestSquashTask_AtomicFailure_RollsBackAllChanges(t *testing.T) {}
func TestSquashTask_PartialCreateTree_NoIndexUpdate(t *testing.T) {}
func TestSquashTask_PartialCreateCommit_NoHEADUpdate(t *testing.T) {}
```

### 5. Rollback Correctness Tests
Append-only enforcement, idempotence.

```go
func TestRollback_Idempotent_SameResultOnSecondCall(t *testing.T) {}
func TestRollback_OriginalRemains_NotDeleted(t *testing.T) {}
func TestRollback_RollbackTwice_IdenticalHash(t *testing.T) {}
```

### 6. Append-Only Enforcement Tests
Verify no DELETE/UPDATE operations on task_commits table.

```go
func TestIndexStore_CreateOnly_NeverUpdates(t *testing.T) {}
func TestIndexStore_CreateOnly_NeverDeletes(t *testing.T) {}
func TestCommitService_NoInPlaceModification_OnlyCreatesNew(t *testing.T) {}
```

### 7. Chain Integrity Tests
Parent hash verification, broken chain detection.

```go
func TestVerifyChain_BrokenLink_ReturnsErrChainBroken(t *testing.T) {}
func TestVerifyChain_ValidChain_ReturnsNil(t *testing.T) {}
func TestVerifyChain_WrongParentHash_ReturnsErrChainBroken(t *testing.T) {}
func TestVerifyChain_CircularReference_ReturnsErrCircular(t *testing.T) {}
```

### 8. go-git Integration Tests
Real git repository operations in temporary directories.

```go
func TestCommitService_IntegrationWithGoGit_CreatesRealCommit(t *testing.T) {}
func TestCommitService_IntegrationWithGoGit_UpdatesRealRef(t *testing.T) {}
func TestSquashService_IntegrationWithGoGit_RealisticScenario(t *testing.T) {}
```

### 9. Concurrency Tests
Race detector, concurrent commits to same branch.

```go
func TestCreateCommit_ConcurrentCommits_RaceDetectorPass(t *testing.T) {}
func TestCreateCommit_ConcurrentToSameBranch_AllSucceed(t *testing.T) {}
func TestCreateCommit_ConcurrentWrites_NoDataRaces(t *testing.T) {}
```

**Run concurrency tests with race detector:**
```bash
go test ./... -race -count=5
```

---

## 9. Test Pyramid

The test distribution should follow this pyramid:

```
         /  E2E Tests  \        (10%)
        /  Integration   \      (20%)
       / Unit Tests (base) \    (70%)
```

### Unit Tests (70% of tests)

- Mock all external dependencies (GitStore, IndexStore, Clock)
- Test service logic in isolation
- Fast (< 1ms per test)
- Deterministic (no flakiness)
- **Example**: `TestCreateCommit_WithValidTaskID_StoresMapping`

```go
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {
    // Mock stores
    gitStore := &mockGitStore{...}
    indexStore := &mockIndexStore{...}
    svc := NewCommitService(gitStore, indexStore, fixedClock)

    // Test in isolation
    commit, err := svc.CreateCommit(context.Background(), req)
    require.NoError(t, err)
    assert.Equal(t, "PROJ-4.2.1", commit.TaskID.Value)
}
```

### Integration Tests (20% of tests)

- Real SQLite + real go-git in temporary directories
- Test Store implementations with real databases
- Test Service + Store together
- Slower (1-100ms per test)
- Still deterministic
- **Example**: `TestCommitService_IntegrationWithGoGit_CreatesRealCommit`

```go
func TestCommitService_IntegrationWithGoGit_CreatesRealCommit(t *testing.T) {
    // Real git repo
    tmpDir := t.TempDir()
    repo, _ := git.PlainInit(tmpDir, false)

    // Real SQLite
    db, _ := setupTempSQLite(t)

    gitStore := NewGoGitStore(repo)
    indexStore := NewSQLiteIndexStore(db)
    svc := NewCommitService(gitStore, indexStore, time.Now)

    commit, err := svc.CreateCommit(context.Background(), validReq)
    require.NoError(t, err)

    // Verify in real git
    obj, _ := repo.CommitObject(plumbing.NewHash(commit.Hash))
    assert.NotNil(t, obj)
}
```

### E2E Tests (10% of tests)

- Full CLI command execution
- Verify all layers together
- Real filesystem, real git, real database
- Slowest (100ms-1s per test)
- Must be rock-solid
- **Example**: `TestCLI_CommitCommand_CreatesTaskCommit`

```go
func TestCLI_CommitCommand_CreatesTaskCommit(t *testing.T) {
    tmpDir := t.TempDir()
    os.Chdir(tmpDir)

    // Initialize repo
    cmd := exec.Command("mgit", "init")
    require.NoError(t, cmd.Run())

    // Create file and commit
    os.WriteFile("main.go", []byte("package main"), 0644)
    cmd = exec.Command("mgit", "commit", "-t", "PROJ-4.2.1", "-m", "initial commit")
    require.NoError(t, cmd.Run())

    // Verify commit exists
    cmd = exec.Command("mgit", "log")
    output, _ := cmd.CombinedOutput()
    assert.Contains(t, string(output), "PROJ-4.2.1")
}
```

---

## 10. Coverage Requirements

Minimum coverage requirements by layer:

| Layer | Line Coverage | Branch Coverage | Rationale |
|-------|---------------|-----------------|-----------|
| Store (go-git + SQLite) | 95% | 90% | Safety-critical data persistence |
| Service (Commit, Squash, Rollback) | 95% | 90% | Safety-critical business logic |
| Model / Validation | 90% | 85% | Core data structures |
| CLI / API / MCP | 90% | 85% | User-facing interfaces |
| **Overall** | **≥ 90%** | **≥ 85%** | **Non-negotiable minimum** |

### How to Check Coverage

```bash
# Generate coverage report
go test ./... -coverprofile=cover.out -count=1

# View coverage by file
go tool cover -func=cover.out

# View specific package
go tool cover -func=cover.out | grep store

# HTML report (visual inspection)
go tool cover -html=cover.out

# Get total coverage percentage
go tool cover -func=cover.out | tail -1
```

### Example Output

```
total:      (statements)    95.3%
```

**95.3% is acceptable (≥ 90%). 85% is NOT acceptable.**

### Checking Branch Coverage

Go's built-in coverage doesn't measure branch coverage. Use additional tools:

```bash
# Install go-cover-treemap (optional, for visualization)
go install github.com/nikolaydubina/go-cover-treemap@latest

# Use gopherbench for more detailed metrics
go test ./... -coverprofile=cover.out && gopherbench cover.out
```

Or manually audit branches in critical code paths to ensure all branches are tested.

---

## 11. Test Infrastructure

Shared test utilities in dedicated packages:

### testutil/fixtures.go
Builders for creating test objects.

```go
package testutil

// CommitBuilder for fluent commit construction
type CommitBuilder struct {
    hash       string
    parentHash string
    taskID     string
    message    string
    timestamp  time.Time
}

func NewCommitBuilder() *CommitBuilder {
    return &CommitBuilder{
        hash:      "defaultHash",
        parentHash: "defaultParent",
        taskID:    "PROJ-1",
        message:   "test commit",
        timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
    }
}

func (b *CommitBuilder) WithHash(h string) *CommitBuilder {
    b.hash = h
    return b
}

func (b *CommitBuilder) WithTaskID(id string) *CommitBuilder {
    b.taskID = id
    return b
}

func (b *CommitBuilder) Build() *model.Commit {
    return &model.Commit{
        Hash:       b.hash,
        ParentHash: b.parentHash,
        TaskID:     model.TaskID{Value: b.taskID},
        Message:    b.message,
        Timestamp:  b.timestamp,
    }
}

// FileDiffBuilder
type FileDiffBuilder struct {
    path    string
    added   []string
    removed []string
}

func NewFileDiffBuilder(path string) *FileDiffBuilder {
    return &FileDiffBuilder{path: path}
}

func (b *FileDiffBuilder) AddedLines(lines ...string) *FileDiffBuilder {
    b.added = lines
    return b
}

func (b *FileDiffBuilder) RemovedLines(lines ...string) *FileDiffBuilder {
    b.removed = lines
    return b
}

func (b *FileDiffBuilder) Build() *model.FileDiff {
    return &model.FileDiff{
        Path:    b.path,
        Added:   b.added,
        Removed: b.removed,
    }
}
```

### testutil/sqlite.go
Temporary SQLite database setup.

```go
package testutil

import (
    "database/sql"
    "testing"
    _ "github.com/mattn/go-sqlite3"
)

// SetupTempSQLite creates a temp SQLite database for testing.
// NEVER use :memory: databases for tests.
func SetupTempSQLite(t *testing.T) *sql.DB {
    tmpFile := t.TempDir() + "/test.db"
    db, err := sql.Open("sqlite3", tmpFile)
    if err != nil {
        t.Fatalf("failed to open sqlite: %v", err)
    }

    // Create schema
    if err := createSchema(db); err != nil {
        t.Fatalf("failed to create schema: %v", err)
    }

    t.Cleanup(func() { db.Close() })
    return db
}

func createSchema(db *sql.DB) error {
    schema := `
    CREATE TABLE IF NOT EXISTS commits (
        hash TEXT PRIMARY KEY,
        parent_hash TEXT,
        message TEXT,
        timestamp INTEGER
    );
    CREATE TABLE IF NOT EXISTS task_commits (
        task_id TEXT,
        commit_hash TEXT,
        PRIMARY KEY (task_id, commit_hash)
    );
    `
    _, err := db.Exec(schema)
    return err
}
```

### testutil/clock.go
Mock clock for time injection.

```go
package testutil

import "time"

// FixedClock returns a clock function that always returns the same time.
func FixedClock(t time.Time) func() time.Time {
    return func() time.Time { return t }
}

// TickingClock returns a clock that increments by 1 second on each call.
func TickingClock(start time.Time) func() time.Time {
    t := start
    return func() time.Time {
        current := t
        t = t.Add(1 * time.Second)
        return current
    }
}
```

### testutil/gitrepo.go
Temporary git repository setup.

```go
package testutil

import (
    "testing"
    "github.com/go-git/go-git/v5"
)

// SetupTempGitRepo creates a temp git repository for testing.
func SetupTempGitRepo(t *testing.T) *git.Repository {
    tmpDir := t.TempDir()
    repo, err := git.PlainInit(tmpDir, false)
    if err != nil {
        t.Fatalf("failed to init git repo: %v", err)
    }
    t.Cleanup(func() { /* auto-cleaned by t.TempDir() */ })
    return repo
}

// AddCommitToRepo creates a test commit in a repository.
func AddCommitToRepo(t *testing.T, repo *git.Repository, filename, content string) string {
    wt, _ := repo.Worktree()
    wt.Filesystem.WriteFile(filename, []byte(content), 0644)
    wt.Add(filename)

    commit, _ := wt.Commit("test commit", &git.CommitOptions{})
    return commit.String()
}
```

### Usage in Tests

```go
func TestCommitService_WithRealGitAndSQLite(t *testing.T) {
    // Setup real git and sqlite
    repo := testutil.SetupTempGitRepo(t)
    db := testutil.SetupTempSQLite(t)
    clock := testutil.FixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

    // Create stores
    gitStore := NewGoGitStore(repo)
    indexStore := NewSQLiteIndexStore(db)
    svc := NewCommitService(gitStore, indexStore, clock)

    // Use builders for test data
    commit := testutil.NewCommitBuilder().
        WithTaskID("PROJ-4.2.1").
        WithHash("abc123").
        Build()

    diff := testutil.NewFileDiffBuilder("main.go").
        AddedLines("func main() {}").
        Build()

    // Test...
}
```

---

## 12. Anti-Patterns

**NEVER do any of the following:**

### ✗ Writing test after production code
This defeats the purpose of TDD. The test should fail first.

**Wrong:**
```go
// First write the implementation
func (s *Service) DoSomething() error { ... }

// Then write a test that passes immediately
func TestDoSomething_ValidInput_Succeeds(t *testing.T) {
    result, err := s.DoSomething()
    require.NoError(t, err)  // Test passes immediately—not testing new behavior
}
```

**Right:**
```go
// First write the failing test
func TestDoSomething_ValidInput_Succeeds(t *testing.T) {
    result, err := s.DoSomething()  // Fails: DoSomething not implemented
    require.NoError(t, err)
}

// Then implement
func (s *Service) DoSomething() error { ... }

// Now test passes
```

### ✗ Test passes immediately (before implementation)
This means the test is testing existing behavior or is too trivial.

**Problem:**
```go
func TestCreateCommit_Succeeds(t *testing.T) {
    svc := NewCommitService(...)
    // If this passes before CreateCommit is implemented, the test is broken.
}
```

**Solution:** Make the test more specific and detailed.

```go
func TestCreateCommit_WithValidTaskID_StoresMapping(t *testing.T) {
    // Now the test fails until CreateCommit is implemented
}
```

### ✗ Testing implementation details instead of behavior
Test behavior, not how it's done.

**Wrong:**
```go
func TestCreateCommit_CallsGitStoreCreateTree(t *testing.T) {
    // Testing internal implementation—don't care how, only what results
    gitStore := &mockGitStore{
        createTreeCalled: false,
    }
    svc.CreateCommit(...)
    assert.True(t, gitStore.createTreeCalled)  // BAD: testing implementation detail
}
```

**Right:**
```go
func TestCreateCommit_WithValidTaskID_CreatesValidCommit(t *testing.T) {
    // Test the outcome, not the implementation
    commit, err := svc.CreateCommit(context.Background(), req)
    require.NoError(t, err)
    assert.Equal(t, "PROJ-4.2.1", commit.TaskID.Value)
    assert.NotEmpty(t, commit.Hash)
}
```

### ✗ Flaky tests (non-deterministic, timing-dependent)
Never rely on timing. Use mocks and fixed clocks.

**Wrong:**
```go
func TestCreateCommit_HasTimestamp(t *testing.T) {
    before := time.Now()
    commit, _ := svc.CreateCommit(context.Background(), req)
    after := time.Now()
    // Flaky: timestamp might be in a different second
    assert.True(t, commit.Timestamp.After(before) && commit.Timestamp.Before(after))
}
```

**Right:**
```go
func TestCreateCommit_HasTimestamp(t *testing.T) {
    expectedTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
    clock := testutil.FixedClock(expectedTime)
    svc := NewCommitService(gitStore, indexStore, clock)

    commit, _ := svc.CreateCommit(context.Background(), req)
    assert.Equal(t, expectedTime, commit.Timestamp)  // Deterministic
}
```

### ✗ Skipping error path tests
Test every error condition.

**Wrong:**
```go
func TestCreateCommit_InvalidInput_ReturnsError(t *testing.T) {
    // Don't skip error tests
    t.Skip("TODO: implement error handling")
}
```

**Right:**
```go
func TestCreateCommit_EmptyTaskID_ReturnsErrInvalidTaskID(t *testing.T) {
    _, err := svc.CreateCommit(context.Background(), &CommitRequest{
        TaskID:  model.TaskID{Value: ""},  // Invalid
        Message: "test",
    })
    assert.Equal(t, model.ErrInvalidTaskID, err)
}

func TestCreateCommit_NoChanges_ReturnsErrNoChanges(t *testing.T) {
    _, err := svc.CreateCommit(context.Background(), &CommitRequest{
        TaskID:  model.TaskID{Value: "PROJ-1"},
        Message: "test",
        Changes: nil,  // Invalid
    })
    assert.Equal(t, model.ErrNoChanges, err)
}
```

### ✗ In-memory SQLite databases
Always use real SQLite files. :memory: is not suitable for testing database state.

**Wrong:**
```go
func TestIndexStore(t *testing.T) {
    db, _ := sql.Open("sqlite3", ":memory:")  // BAD: in-memory
    store := NewSQLiteIndexStore(db)
    // ...
}
```

**Right:**
```go
func TestIndexStore(t *testing.T) {
    db := testutil.SetupTempSQLite(t)  // Real file in t.TempDir()
    store := NewSQLiteIndexStore(db)
    // ...
}
```

### ✗ Hardcoded file paths in tests
Use t.TempDir() for all temporary files.

**Wrong:**
```go
func TestGitStore(t *testing.T) {
    repo, _ := git.PlainInit("/tmp/my-test-repo", false)  // BAD: hardcoded
    // ...
}
```

**Right:**
```go
func TestGitStore(t *testing.T) {
    tmpDir := t.TempDir()  // Auto-cleaned
    repo, _ := git.PlainInit(tmpDir, false)
    // ...
}
```

---

## Summary

The TDD workflow for mgit is strict and non-negotiable:

1. **READ** requirements from REQUIREMENTS.md
2. **WRITE** failing test (confirm it fails)
3. **RUN** test → CONFIRM RED
4. **WRITE** minimum production code
5. **RUN** test → CONFIRM GREEN
6. **REFACTOR** (keep tests green)
7. **CHECK** coverage ≥ 90%
8. **COMMIT** test and production code together

Follow the naming conventions, use table-driven tests, test all categories (happy path, error paths, boundaries, atomicity, idempotence, chain integrity, integration, concurrency), and maintain the test pyramid (70% unit, 20% integration, 10% E2E).

**Coverage is non-negotiable: ≥ 90% line coverage, ≥ 85% branch coverage, minimum.**

**If it's not tested, it doesn't work.**
