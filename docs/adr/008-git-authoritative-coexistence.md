# ADR-008: Git-Authoritative Coexistence and Auto-Housekeeping

**Status:** Accepted
**Date:** 2026-06-26
**Refs:** MGIT-35, MGIT-14 (mgit-over-git coexistence), ADR-001 (Embedded git), ADR-007 (Linked-worktree binding), MGIT-26 (dogfood), MGIT-32 (.gitignore), MGIT-34 (`mgit work`)

## Context

mgit runs **over** an existing git repository (MGIT-14, ADR-001): the project's
`.git` and mgit's `.mgit` are two separate stores. Dogfooding mgit on its own
repo (MGIT-26) surfaced a drift problem: work delivered **via git** (plain
`git commit`) advances git's tree while `.mgit` stays at its last import, so
`mgit work` silently materialized a **stale** worktree until a manual
`mgit add . && mgit commit` resync. The drift was silent and easy to miss.

Two questions had to be answered: **(1)** which store is the source of truth,
and **(2)** how is the other kept coherent without burdening the user?

A safety/security review of the first-cut answer ("follow live git `HEAD` and
re-import the working tree on access") found it would **silently mis-land a
hotfix**: a `git switch hotfix` would retarget mgit's base, so an agent's
`main`-intended work would land on `hotfix` with a garbage diff — plus base
thrash on detached-HEAD/bisect/rebase, dirty-tree pollution, and false-negative
drift detection. This ADR records the corrected model.

## Decision

### 1. Git is authoritative; there is no "mgit-as-substrate" mode

The project's **git is the source of truth**. `.mgit` is the agent's
*checkpointed working layer* over git; finished work flows back to git via the
land path (`mgit squash --to-git`). This is required, not optional — there is
**no opt-in mode** where mgit owns history and git is downstream. (Mixing the
two is exactly what caused the MGIT-26 drift.)

### 2. The base anchors on a designated integration ref — NOT live `HEAD`

mgit's base tracks a **configured integration ref** (default: the repository's
default branch, e.g. `main`), read from `.git` **read-only**. Branch switches,
hotfix checkouts, detached HEAD, `bisect`, and in-progress `rebase` **do not
move mgit's base**. This is the single most important correction: anchoring on
live `HEAD` is what would silently mis-land a hotfix.

Deliberate hotfix/release work targets a base explicitly and **pinned**:
`mgit work --task <ID> --base <ref>`. Working a hotfix is then a choice, never
a side effect of what happens to be checked out.

The base syncs from the ref's **committed tree**, never the dirty working tree
(no uncommitted, possibly-unrelated edits pollute the base).

### 3. Auto-housekeeping — no manual `mgit sync`

mgit keeps the base coherent **automatically**. There is no user-facing
`mgit sync` chore (a missed sync is a footgun). On every base-dependent command
(`work`/materialize, `status`, `diff`, the squash base) mgit:

1. runs a **cheap, content-based drift check** — compares the integration ref's
   current commit id (read-only) against the id `.mgit` last synced from. (A
   commit-id/content signal, **not** mtime, which can false-negative.)
2. if — and only if — they differ, **auto-resyncs** the base from the ref's
   committed tree (reusing the import path, `.gitignore`-honoring per MGIT-32).

The common path is the cheap compare; a full re-import runs only on real drift.
It **fails loud** — it never materializes or diffs against a *known-stale*
base.

### 4. Each task pins its fork-base

A task records the base commit it forked from. `squash`/`diff` are always
computed against that **pinned** fork-base, so a later base move never corrupts
a task's net change.

### 5. Per-store isolation for parallel agents; the host store is the integration point

Parallel agents do **not** share one `.mgit`. Each sandbox already gets a
**fresh private `.mgit`** (SEC-03; `internal/sandboxd/provision`), and the host
shared store is provably unreachable from a guest. The host `.mgit` is the
*integration point* only: it seeds each sandbox's private store, **receives**
verified commits via the land path (already serialized — single-flight per
sandbox, MGIT-11.13.5), and holds the unified ledger/audit. Work converges at
land, so per-store isolation does not fragment provenance.

Concurrent direct writes to one shared store only arise in the lightweight
non-sandbox path (multiple `mgit work` linked worktrees writing the host store
directly, mirroring `git worktree` sharing `.git`). For that path, resync and
commit are **transactional and lock-guarded** within `.mgit`, and append-only
is preserved (resync appends base commits, never rewrites).

### 6. Read-only coexistence

ADR-001/MGIT-14's invariant is clarified: mgit **never mutates** `.git`, but it
**may read** it (refs, the integration-ref tree) read-only to know git's truth.
`.git` reads are defensive: handle the `.git`-as-**file** gitdir pointer (linked
git worktrees, submodules), symlinks, sparse-checkout, shallow clones, and
git-LFS. The existing `.git`-untouched tests still hold — they assert no
*mutation*.

## Consequences

**Prevents** (per the safety review): silent wrong-branch hotfix land,
base thrash on branch-switch/detached-HEAD/bisect/rebase, dirty-tree base
pollution, corrupted squash diffs after a base move, false-negative drift,
torn/partial base under concurrency, and silent ingestion of tampered
working-tree files (resync is from the committed integration-ref tree, a
reviewed point, not an arbitrary dirty tree).

**Costs:** per-store isolation copies the base lineage per private store (disk;
already accepted for sandboxes). Reading `.git` couples base-derivation (not all
commands) to git readability — degrade gracefully, fail loud only where the base
is actually needed.

**Rejected alternatives:** (a) *follow live `HEAD` + reimport the working tree* —
silently mis-lands hotfixes (the breaker above). (b) *manual `mgit sync`* —
a footgun: a missed sync corrupts silently. (c) *mgit-as-authoritative / opt-in
substrate mode* — doubles the source-of-truth surface and is what caused the
MGIT-26 drift; dropped.

## Implementation

Tracked in **MGIT-35**: the integration-ref anchor + cheap content drift gate +
auto-resync routine wired into `work`/`status`/`diff`/squash; per-task pinned
fork-base; transactional+locked resync; `--base <ref>`; defensive `.git` reads;
tests (drift auto-healed, clean path is a cheap no-op, never materializes stale,
`.git` never mutated); a perf check on the gate; and README + the MGIT-29 agent
skill documenting "mgit keeps itself in sync with git — no manual step."
