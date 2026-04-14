# mgit Branch Strategy Specification

## Purpose

Define how mgit branches map to mtix issues and how micro-commits flow through the version control system. This document establishes the branch model for safety-critical micro VCS integration with mtix task management.

## Core Concepts

### 1. Branch-per-Issue Model

Every mtix issue automatically gets an mgit branch named according to the pattern:
```
task/{PROJECT}-{DOTTED_ID}
```

Examples:
- `task/PROJ-4.2.1` for issue PROJ-4.2.1
- `task/CORE-1.0.0` for issue CORE-1.0.0
- `task/SAFETY-7.3.2.1` for issue SAFETY-7.3.2.1

This naming convention ensures:
- Direct traceback from branch to mtix issue
- Namespace isolation for concurrent work
- Automatic discovery of issue-related branches via glob patterns

### 2. Micro-Commits Stack on Issue Branch

When an LLM agent works on a micro-issue (e.g., PROJ-4.2.1.3), all commits are written to the parent issue branch `task/PROJ-4.2.1`. This allows:
- Multiple micro-issues within a single parent issue to accumulate commits
- Linear commit history per issue branch
- Clear parent-child relationships in both mtix and mgit

**Example:**
```
Micro-issue PROJ-4.2.1.1 → commits [c1, c2]
Micro-issue PROJ-4.2.1.2 → commits [c3]
Micro-issue PROJ-4.2.1.3 → commits [c4, c5]

All land on branch: task/PROJ-4.2.1
Commit history: [c1]--[c2]--[c3]--[c4]--[c5]
```

### 3. Main Branch: Squashed Only

The `main` branch contains ONLY squashed commits—one per completed mtix issue.

**Invariants:**
- Every commit on `main` is a squash commit (tagged in metadata)
- Commit message format: `squash({taskID}): {summary}`
- Parent of each main commit is the previous main commit (linear history)
- No merge commits on main
- No direct commits to main

**Purpose:**
- Clean, auditable history of completed work
- Each main commit is a self-contained, reviewable unit
- Easy to identify what was shipped per issue

### 4. Branch Lifecycle

```
Created
   ↓
task/{PROJECT}-{ID} branch exists
   ↓ (commits accumulate from micro-issues)
Active (has open micro-issues)
   ↓ (issue completed, all micro-issues closed)
Ready for squash
   ↓ (mgit squash --task {PROJECT}-{ID} --to-main)
Squashed to main
   ↓ (optional cleanup if --cleanup flag set)
Deleted (or archived)
```

**State transitions:**
- **Created**: Automatically when first commit targets an mtix issue
- **Active**: Branch exists, commits being added
- **Ready for squash**: No open micro-issues remain, branch has commits not yet squashed
- **Squashed**: Squash commit created on main, original commits still exist on task branch
- **Cleaned**: Branch deleted after successful squash (optional, configurable via `mgit.cleanup.auto_delete_branches`)

### 5. Fast-Forward Merge Only

Squash operations merge to `main` using fast-forward merge only.

**Algorithm:**
1. `mgit squash --task PROJ-4.2.1 --to-main`
2. Create squash commit on `task/PROJ-4.2.1`
3. Attempt fast-forward merge: `main` → squash commit
4. If `main` has diverged (is not an ancestor):
   - Abort with ErrMainDiverged
   - User must manually rebase: `mgit rebase task/PROJ-4.2.1 --onto main`
   - Retry squash

**Rationale:**
- Guarantees linear main branch history
- Prevents accidental loss of commits via merge commits
- Forces explicit user awareness of conflicts

### 6. Orphan Branch Detection

`mgit verify --branches` scans for orphaned or stale branches:

**Criteria:**
- Branches with no commits in 30+ days (configurable)
- Branches with no associated open mtix issues
- Branches that reference deleted issues

**Output:**
```
mgit verify --branches

WARNING: Orphaned branches (no commits for 30+ days):
  task/PROJ-4.2.1       last commit: 2026-02-01T14:23:00Z (36 days ago)
  task/PROJ-4.3.0       last commit: 2026-02-05T08:10:00Z (32 days ago)

SUGGESTION: Delete with:
  mgit branch -d task/PROJ-4.2.1
  mgit branch -d task/PROJ-4.3.0

Or use: mgit cleanup --all --older-than 30d
```

### 7. Branch Naming Rules

**Mandatory pattern for issue branches:**
```
^task/[A-Z][A-Z0-9]+-[0-9]+(\.[0-9]+)*$
```

Examples:
- `task/PROJ-1` ✓
- `task/PROJ-1.0.1` ✓
- `task/SAFETY-7.3.2.1` ✓
- `task/proj-1` ✗ (lowercase project prefix)
- `task/PROJ-abc` ✗ (non-numeric ID)

**Manual/experimental branches:**
- Allowed with prefix: `manual/` or `experiment/`
- Example: `manual/feature-xyz`, `experiment/refactor-auth`
- Not synced to mtix automatically
- Can be deleted without ceremony

**Protected branches:**
- `main`: cannot be deleted, force-pushed, or have commits directly pushed
- `task/*`: cannot be force-pushed (rebase-only)
- Operations that would violate protection return ErrBranchProtected

### 8. Concurrent Branches

Multiple agents can work simultaneously on different issue branches without coordination:

```
Agent A: working on task/PROJ-4.1
Agent B: working on task/PROJ-4.2
Agent C: working on task/PROJ-4.3

All three can commit in parallel — no locking needed.
```

**Same-branch concurrent work:**
If two agents attempt to commit to the same branch (e.g., `task/PROJ-4.1`), the second agent's push is rejected with ErrConcurrentBranchModification.

**Resolution options:**
1. Rebase second agent's work onto current branch tip
2. Explicit locking: `mgit lock --task PROJ-4.1 --agent AgentB` (see FR-5.4)
3. Wait for first agent to complete task

## Branch Visualization

```
Squashed commits on main (linear):
main:          [S1]---[S2]---[S3]---[S4]

Micro-commits on task branches (stacked):
task/PROJ-4.1: [m1][m2][m3]              (3 micro-commits)
               squash ↓↓↓
               creates S1 on main

task/PROJ-4.2:         [m4][m5][m6][m7]  (4 micro-commits)
                       squash ↓↓↓↓
                       creates S2 on main

task/PROJ-4.3:                [m8][m9]   (2 micro-commits)
                              squash ↓↓
                              creates S3 on main

task/PROJ-4.4:                    [m10][m11][m12]
                                  (not yet squashed)
```

**Legend:**
- `S` = Squashed commit (on main)
- `m` = Micro-commit (on task/* branch)
- Each micro-commit becomes part of a squash commit eventually

## Branch Operations

### Creating a branch

Automatic on first commit to an mtix issue. Manual creation:
```bash
mgit branch task/NEW-1 --from main
```

### Listing branches

```bash
mgit branch --list                    # all branches
mgit branch --list --active          # branches with open issues
mgit branch --list --pattern "PROJ-*" # glob filter
```

### Deleting a branch

```bash
mgit branch -d task/PROJ-4.2.1         # safe delete (requires no divergence from main)
mgit branch -D task/PROJ-4.2.1         # force delete (only if you're absolutely sure)
```

### Rebasing a task branch

If main has advanced and you need to rebase:
```bash
mgit rebase task/PROJ-4.1 --onto main
```

This moves all commits on `task/PROJ-4.1` to start from the current main tip.

## Integration with mtix

**Automatic synchronization:**
- New mtix issue → mgit branch created automatically
- Micro-issue added → mgit receives notification, branch remains active
- Issue marked "Ready for Merge" → `mgit squash --task ... --to-main` recommended
- Issue closed → branch marked for cleanup (if configured)

**Branch-to-Issue lookup:**
```
GET /mtix/api/issue/PROJ-4.2.1
  ↓
Returns: { issue_id, branch: "task/PROJ-4.2.1", status: "active", ... }

GET /mgit/api/branch/task/PROJ-4.2.1
  ↓
Returns: { branch_name, task_id: "PROJ-4.2.1", commit_count: 7, ... }
```

## Summary

The branch strategy establishes:
1. **1:1 mapping** between mtix issues and mgit branches
2. **Linear accumulation** of micro-commits on task branches
3. **Squashed main** containing one commit per completed issue
4. **Concurrent-safe** parallel work on different issues
5. **Audit-friendly** branch history and lifecycle tracking
