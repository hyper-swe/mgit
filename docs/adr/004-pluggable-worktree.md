# ADR-004: Pluggable Worktree Implementation Strategy

**Status:** Accepted  
**Date:** 2026-03-12  
**Refs:** FR-16 (Agent Worktrees)

---

## Context

mgit needs to support multiple linked worktrees so that parallel LLM coding agents can work on different tasks simultaneously within the same repository. The primary users are LLM agents trained on standard git documentation — they already understand `git worktree add`, `git worktree list`, and `git worktree remove`.

Three implementation options were evaluated:

1. **Native go-git v6 worktrees** — go-git v6 ships a `x/plumbing/worktree` package with `Add()`, `List()`, `Remove()`, and `Open()`.
2. **mgit-managed worktrees** — mgit implements worktree semantics at the application level, managing isolated directories, HEAD files, and task bindings through its own code.
3. **Pluggable interface with mgit v1 implementation** — Define a `WorktreeManager` interface, implement it at the application level for v1, and swap to a go-git v6 backed implementation later when v6 matures.

## Decision

**Option 3: Pluggable interface with mgit v1 implementation.**

The CLI mirrors standard git worktree commands (`mgit worktree add`, `list`, `remove`, `prune`) so that LLM agents can use familiar semantics. The implementation is behind a `WorktreeManager` interface, allowing the backend to be swapped without changing the CLI, MCP, service, or API layers.

## Rationale

### Why not native go-git v6 now?

- go-git v6 was published **February 23, 2026** — less than three weeks before this decision. It has ~30 known importers compared to v5's ~4,756.
- The worktree package lives under `x/` (experimental prefix in Go convention). It has not graduated to the stable API surface.
- Known open bugs: sparse-checkout status incorrect (#1406), `RemoveGlob` fails on directories (#1190), `Add` previously included `.git` in index (#814).
- For a safety-critical project, adopting an experimental API from a weeks-old major release as a core architectural dependency is not acceptable.

### Why not pure mgit-managed?

- Locks mgit into a custom abstraction permanently.
- If go-git v6 matures (6-12 months), migrating from a non-abstracted implementation requires rewriting internals.
- No path to benefiting from the go-git community's testing and maintenance of worktree mechanics.

### Why pluggable?

- **LLM agents see git-compatible commands.** `mgit worktree add --task PROJ-4.2.1 ./worktrees/agent-a` behaves as agents expect from git training data.
- **v1 works on go-git v5.** No dependency upgrade needed. The implementation manages `.mgit/worktrees/` directories, HEAD files, and task bindings through mgit's existing storage layer.
- **v2 migration is a backend swap, not a rewrite.** Once go-git v6 stabilizes, a new `WorktreeManager` implementation can delegate to v6's native API. The CLI, MCP, service, and API layers are untouched.
- **Task binding remains in mgit regardless.** Even with a v6 backend, mgit still manages the task-to-worktree mapping in `index.db` — this is mgit-specific and would never be in go-git.

## Interface Contract

```go
type WorktreeManager interface {
    Add(ctx context.Context, opts WorktreeAddOptions) (*WorktreeInfo, error)
    List(ctx context.Context) ([]WorktreeInfo, error)
    Remove(ctx context.Context, path string, force bool) error
    Prune(ctx context.Context, dryRun bool) ([]string, error)
    Resolve(ctx context.Context, path string) (*WorktreeInfo, error)
}
```

## v1 Implementation Details

- Worktree metadata stored in `.mgit/worktrees/<name>/` (HEAD, task_binding, agent_id, created_at, locked)
- Worktree registry in `index.db` (`worktrees` table with path, name, branch, task_id, agent_id, created_at, last_commit_at)
- Working directories created at user-specified paths (typically `./worktrees/<agent-name>/`)
- Branch isolation enforced: no two worktrees may share a branch or task ID
- Object store and refs are shared across all worktrees (same `.mgit/objects/` and `.mgit/refs/`)

## v2 Migration Criteria

The go-git v6 native backend should be considered when ALL of the following are met:

1. go-git v6 has been published for at least 6 months
2. The `x/plumbing/worktree` package has been promoted out of `x/` or has at least 100 importers
3. Known worktree bugs (#1406, #1190) are resolved
4. At least one production system publicly reports using v6 worktrees

Until these criteria are met, v1 remains the active implementation.

## Consequences

### Positive
- LLM agents get a familiar CLI surface from day one
- Future go-git v6 adoption is a backend swap, not a redesign
- Task binding logic stays clean regardless of backend

### Negative
- mgit v1 carries additional implementation and testing burden for worktree mechanics
- The `WorktreeManager` interface adds one level of indirection
