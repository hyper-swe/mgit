# ADR-013: mgit-generated worktree scaffolding is excluded from bulk staging by recorded provenance

**Status:** Accepted
**Date:** 2026-08-10
**Refs:** MGIT-80, MGIT-77, MGIT-32, MGIT-47, FR-16, FR-2, MGIT-11.11.1, MGIT-11.11.3

## Context

`mgit work <path> --task <ID>` does not just create a worktree — it writes
mgit's own agent scaffolding into it:

| Path | Written when | Shape |
|---|---|---|
| `CLAUDE.md` | always | marked block upserted into a possibly-existing file |
| `.claude/settings.json` | containment requested/active | JSON merge into a possibly-existing file |
| `AGENTS.md` | containment requested/active | marked block upserted |
| `.cursor/rules/mgit-sandbox.mdc` | containment requested/active | mgit-owned, written wholesale |
| `.envrc` | containment requested/active | marked block upserted |
| `.mgit/shims/*` | containment requested/active | already under `.mgit/`, never staged |

None of it is the task's work. All of it is worktree-specific and
host-specific: the generated block names *this* worktree's path and task
binding and states whether *this* machine has a sandbox. Landed into the
user's repository it is not merely noise, it is wrong at the destination.

Until MGIT-77 the prescribed agent loop was a bare `mgit commit`, which
recorded only what had been staged by hand — so agents simply never staged the
scaffolding, and two of them independently reported working around it. MGIT-77
correctly replaced that loop with `mgit commit -a` (a plain `mgit commit`
recorded *nothing*, which was the worse bug). `-a` stages every change,
including new files. From that point on, the instruction mgit itself generates
guarantees mgit's own scaffolding lands in the user's repository.

The fix must be in mgit. "Remember not to stage CLAUDE.md" is an instruction
that only works when the agent recalls an exception — exactly the failure mode
MGIT-77 closed.

The bug has **two shapes**, and a fix that handles only one is half a fix:

1. **Fresh project** — `CLAUDE.md` is a brand-new untracked file.
2. **Project that already has a `CLAUDE.md`** (mgit-dev itself) — mgit's block
   is an **edit to a tracked file**.

## Decision

### 1. mgit records the provenance of what it generated, per worktree

At generation time, `mgit work` writes the worktree-relative paths it actually
produced into `<worktree>/.mgit/generated` — a newline-delimited,
`#`-commentable manifest, alongside the existing `worktree` marker and
`seed-include` list. `.mgit/` is already excluded from staging, so the record
itself can never land.

Only paths that exist on disk after the writers ran are recorded. The adapter
writes are best-effort (they warn, they do not fail worktree creation), and
mgit must not claim a file it never wrote — otherwise a user's own later
`.envrc` would be silently unstageable.

`agentadapter.GeneratedWorktreeFiles(contained)` is the single declaration of
what mgit generates; the writers resolve their paths from the same constants,
and a test asserts the declared set equals the set the writers actually produce
on disk. Adding a new generated file without declaring it fails that test
rather than silently landing in someone's repository.

### 2. The exclusion is applied at BULK staging, not at file visibility

`WorktreeStore.addAll` — the single choke point behind `mgit add -A`,
`mgit add .` and `mgit commit -a` — skips recorded paths.

Applying it there is what makes both shapes fall out of one rule: bulk staging
is orthogonal to whether a path is tracked. `mgit status` continues to report
the truth (the file *is* modified) — mgit does not hide a real working-tree
change from the reviewer.

### 3. An explicit pathspec always wins

`mgit add CLAUDE.md` stages it. A named path is an unambiguous statement of
intent; a bulk stage means "everything I touched", and the agent did not touch
mgit's scaffolding — mgit did. This mirrors git's own asymmetry, where
`git add -f <path>` overrides `.gitignore`. A path staged explicitly also
survives a subsequent `mgit commit -a`: a bulk stage never *retracts* a
deliberate selection.

So a user who wants to commit their own CLAUDE.md edits can, with one extra
word, and the failure mode of forgetting is the safe direction (their edit is
not committed yet) rather than the unsafe one (mgit's scaffolding lands).

It is honored, but it is **not silent**. Because the user's directive and
mgit's generated block live in the same file, staging the path commits both.
`mgit add CLAUDE.md` therefore prints a note saying the path is mgit-generated,
that the block is worktree- and host-specific, and that `mgit commit -a` alone
would have skipped it. A warning is the right instrument here rather than a
refusal: the caller named the path, and mgit must not be in the business of
deciding a user may not commit their own file.

## Alternatives considered

**A hard-coded filename blocklist in the staging walk.** Rejected: it encodes
"CLAUDE.md/AGENTS.md/.envrc belong to mgit", which is false — projects track
their own, and would find them permanently un-bulk-stageable inside every
worktree, including for legitimate edits. The fact being encoded is provenance
("mgit generated this file, here"), not a name.

**Append the paths to the project's `.gitignore`.** Rejected: it writes into
*user* content, and that write itself lands in the patch — the same defect one
level up. It is also repository-global rather than worktree-local, and
gitignore does not retract already-tracked files, so shape 2 would still land.

**A `.git/info/exclude`-style ignore file under `.mgit/`, fed into
`Repository.ignoreMatcher` (MGIT-32).** Attractive — it reuses machinery that
already exists and writes nothing into user content — but insufficient as the
carrier, for the same reason: gitignore semantics only cover untracked paths.
Filtering a *tracked* path out of `listWorkingFiles` would make `Status` classify
it as DELETED (the "present at HEAD, absent on disk" branch) and a bulk stage
would then stage the **removal** of the user's real CLAUDE.md — strictly worse
than the bug being fixed.

**Fix it in the generated instructions** ("remember not to stage CLAUDE.md").
Rejected on the ticket's own terms: an instruction that works only when the
agent remembers an exception is the failure mode MGIT-77 closed.

## Consequences

- A worktree created before this change has no manifest, so nothing is
  excluded — the exclusion is opt-in per worktree by construction, and no
  existing repository changes behavior. Re-running `mgit work`, or launching a
  sandbox (which regenerates the CLAUDE.md block), records it.
- `mgit status` still lists the generated files as modified/untracked. That is
  deliberate honesty, but it does mean a worktree is never reported "clean"
  while mgit's block differs from the committed content — which, for a tracked
  `CLAUDE.md`, also means the store-level checkout guard
  (`dirtyTrackedPaths`) still counts it. That pre-existed this change and is
  left alone here rather than widened into a second exclusion surface.
- A failure to write the manifest is warned about with its consequence named,
  not swallowed — but it does not abort worktree creation, matching the other
  best-effort wiring legs.
