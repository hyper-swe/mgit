# ADR-007: Linked-Worktree Store Binding and Per-Worktree HEAD

**Status:** Accepted
**Date:** 2026-06-23
**Refs:** FR-16 (Agent Worktrees), ADR-004 (Pluggable Worktree Strategy), ADR-001 (Embedded git / coexistence), MGIT-17, MGIT-24

## Context

`mgit worktree add` (FR-16) creates a linked worktree for an agent: a task branch,
a registry entry binding the worktree to exactly one task, the agent adapters, and —
since MGIT-17 — the branch's source materialized into the worktree directory.

But running `mgit` *from within* a worktree did not work. Two gaps (MGIT-24):

1. **Store discovery** — `openAppFromCwd` looked for `.mgit` only in the exact cwd,
   so any command from a subdirectory failed. Fixed first (MGIT-24 slice 1): `mgit`
   now walks up to the nearest `.mgit` directory, like git finds `.git`.

2. **No per-worktree HEAD** — `Repository` has a single `root` and the store's single
   shared `.mgit/HEAD`. Two worktrees on different task branches cannot share one HEAD,
   so worktree-local commits on the bound branch were impossible. This ADR addresses (2).

The design always intended worktree-local commits: ADR-004 says the v1 implementation
"manages `.mgit/worktrees/` directories, HEAD files, and task bindings," and CLAUDE.md's
WORKTREE CONVENTIONS state "Commits from within a worktree auto-inherit the bound task
ID." The linkage was simply never implemented — MGIT-17 only materialized files.

## Decision

A linked worktree shares the parent's `.mgit` **object store + refs + index.db** but
operates on its **own bound branch** as HEAD, with working files rooted at the worktree
directory. This is git's worktree model (shared objects, per-worktree HEAD).

### Marker

`mgit worktree add` writes a self-contained marker at `<worktree>/.mgit/worktree` (JSON):

```json
{ "store": "<abs path to the parent project's .mgit>", "branch": "task/<ID>", "task": "<ID>" }
```

The worktree's `.mgit` stays a **directory** (it also holds the MGIT-11.11 agent shims),
so the MGIT-24 walk-up finds it. `OpenApp` distinguishes a linked worktree from a real
store by the presence of the `worktree` marker file inside `.mgit/`.

### Per-worktree HEAD (the crux)

`Repository` gains an optional `branchOverride` field. All "what is the current
branch/HEAD" resolution funnels through one accessor, `currentRef()`:

- override set (linked worktree) → resolve `refs/heads/<branchOverride>` from the shared
  store (a hash reference; the shared `.mgit/HEAD` is **never read or written**);
- override empty (normal repo) → `repo.Head()` as before.

`Head()`, `CurrentBranch()`, `headFiles()`, `CommitStore.CreateCommit`, and
`MergeStore.CreateMergeCommit` all use `currentRef()` instead of `repo.Head()`. A commit
from a worktree therefore advances the bound branch ref (via the existing CAS) and never
mutates the shared HEAD — so two worktrees on disjoint branches never collide.

### Linked open

`OpenLinked(worktreeRoot, parentMgitPath, branch)` opens the go-git store at
`parentMgitPath` (shared objects/refs) but sets `root = worktreeRoot` (working-file reads
+ materialization target the worktree) and `branchOverride = branch`. `OpenApp` opens the
**parent's** `index.db`, audit log, and config, and acquires the file lock on the
**parent** `.mgit` — so concurrent ops across the parent and its worktrees serialize on
one lock (preserving the single-writer invariant; SQLite WAL + CAS refs are belt-and-
suspenders).

### Auto task inheritance

`OpenApp` exposes the marker's bound task on the `App`. `mgit commit` with no `--task-id`,
inside a worktree, defaults to the bound task (CLAUDE.md). An explicit `--task-id` that
contradicts the binding is rejected (ErrTaskMismatch).

### Branch switching in a worktree

A worktree is bound to one branch for its lifetime (FR-16: no task/branch sharing).
`checkout`/branch-switch from within a linked worktree is rejected — the binding is fixed.

## Consequences

- `mgit add/commit/log/status` work from within a worktree, on the bound branch, sharing
  the parent store; the project `.git` is never touched (ADR-001 / MGIT-14 invariant holds
  — only `.mgit` and working files under the worktree root are read/written).
- HEAD resolution is centralized in `currentRef()`, removing scattered `repo.Head()` calls
  from the commit/merge paths — a net clarity gain.
- v1 keeps the binding in an in-worktree marker rather than ADR-004's parent-side
  `.mgit/worktrees/<name>/` tree. The marker is self-contained (no registry round-trip at
  open) and matches MGIT-17's in-worktree `.mgit/` layout. Migrating to the parent-side
  layout later is internal to `OpenApp`/the marker reader and does not change the CLI.
- The full git semantics of multiple HEADs per ref, detached HEAD in a worktree, and
  `worktree move`/`repair` remain out of scope (FR-16 binds one branch per worktree).
