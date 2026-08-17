# ADR-009: Per-Operation Repo Locking for Long-Lived Operations

**Status:** Accepted (amended 2026-08-16, MGIT-120; 2026-08-17, MGIT-122)
**Date:** 2026-07-03
**Refs:** MGIT-46, MGIT-120, MGIT-122, MGIT-10 (process lock), ADR-001 (embedded git)

## Context

Every mgit process serializes on one advisory file lock —
`.mgit/locks/mgit.lock`, an exclusive `flock(2)` acquired in `OpenApp` and held
until the process closes its stores (MGIT-10, `internal/store/lock`). The
original decision below reasoned that this is exactly right for a CLI command,
because the lock is then held for the command's (short) lifetime.

> **Amendment (2026-08-16, MGIT-120): "a CLI command is short" was an
> assumption, and it was wrong.** `mgit work` — the command every parallel
> agent starts with — held the lifetime lock across a full working-tree
> fingerprint, a whole worktree materialization and, with `--sandbox`, a
> round-trip to a daemon that may be booting another agent's VM. The fleet soak
> measured four concurrent provisions on a loaded repo: one held the lock for
> more than thirty seconds and the waiters died with the very error this ADR was
> written to eliminate. See "Amendment" below for the corrected rule.

`mgit serve` broke the assumption. It opens the same `App` and holds the
lifetime lock for **the whole life of the server**. An external trial (2026-07-03)
ran `mgit serve --mcp-only` as an agent's MCP server for ~a day; every CLI
command on that repo then failed after a 30-second wait:

```
another mgit process is running: held by PID <serve>
```

So a long-lived MCP/REST server and the CLI were mutually exclusive by
construction — which defeats the standard pattern of an agent driving mgit over
MCP while a human (or a second agent) uses the CLI on the same repo.

## Decision

**The server does not hold the process lock for its lifetime. It acquires the
lock per operation, for the duration of that operation only — the same scope a
CLI command holds it.**

Concretely:

1. **`App.DetachLock()`** releases the lifetime lock the `App` acquired at open
   and returns a `lock.Guarder` bound to the same store directory. `mgit serve`
   calls it right after opening the app. From then on the `App` no longer owns
   the lock (its `Close` will not release it).

2. **`lock.Guarder.Guard(fn)`** acquires a fresh `flock`, runs `fn`, and
   releases — always, including on error. It is injected into both server
   surfaces as middleware:
   - MCP: a `ToolHandlerMiddleware` wraps every tool call (`WithLocker`).
   - REST: an Echo middleware wraps every request (`WithLocker`).

3. **Why a fresh acquire per call also serializes in-process requests.** `flock`
   locks are per *open file description*. Two concurrent server requests each
   `open()`+`flock()` the lockfile, so the second blocks on the first — even
   within one process. The server therefore needs no separate in-process write
   mutex; the same mechanism serializes serve-vs-CLI and request-vs-request.

4. **Failure is fast and named.** If the lock cannot be acquired within the
   timeout, the operation returns a structured error (MCP tool error / HTTP 503)
   instead of hanging. The lockfile now records the holder's command on a second
   line (`PID\ncommand`), so a contended-lock error names *which* command holds
   it (e.g. `held by PID 4213 (mgit serve)`), not just a PID.

## Consequences

- **Server and CLI coexist.** With the server per-operation-locking, a CLI
  `status` / `commit` / `worktree add` acquires the lock in the gap between
  server operations. No 30-second hang; no lifetime starvation.
- **Correctness is unchanged.** At most one holder of the exclusive lock exists
  at any instant, exactly as before — the *scope* shrank from "server lifetime"
  to "one operation." SQLite WAL + `busy_timeout` and go-git object/ref
  atomicity remain the underlying store guarantees; the process lock continues
  to serialize writers on top of them.
- **Uniform guarding (reads included), for now.** Every server operation is
  guarded, not just writes. This is the simplest obviously-correct rule and,
  because each guarded operation is short, contention is brief. Reads taking a
  *shared* lock (or none) is a possible future optimization (`flock` supports
  `LOCK_SH`); it is deliberately out of scope here to keep the change small and
  the correctness argument trivial.
- **CLI is untouched.** Only `serve` detaches; every CLI command keeps holding
  the lock for its command lifetime, so its single-writer behavior is identical.
  *(Superseded by the amendment below: the provisioning commands detach too.)*

## Alternatives considered

- **CLI proxies to the running server when the lock is held.** Heavier: it
  couples the CLI to a running server's transport and lifecycle, and does
  nothing when no server is running. Per-operation locking needs no coordination
  and works whether or not a server is up.
- **A separate in-process RW mutex in the server plus the lifetime flock.** Would
  fix in-process races but not the cross-process CLI starvation, which is the
  actual reported bug.

---

## Amendment (2026-08-16) — the rule is about DURATION, not about `serve`

**Refs:** MGIT-120

### What went wrong

The original decision drew the line in the wrong place: around the *kind* of
process (server vs. CLI) rather than around *how long the work takes*. `mgit
work` is a CLI command, so it kept the lifetime lock — and it is the least
short command mgit has. Inside one critical section it ran:

1. `SyncService.EnsureSyncedForNewWorktree` → `WorkingTreeFingerprint`, which
   reads and SHA-256s every working file;
2. `WorktreeStore.MaterializeBranchTo`, which inflates and writes every blob of
   the branch tree to disk, serially;
3. with `--sandbox`, `EnsureRunning` + `Launch` against `mgit-sandboxd`, whose
   own service mutex was, at the time, also held across a full VM boot (the
   same defect one layer down — see the second amendment, MGIT-122).

The tell was already in the tree: `mgit sandbox launch` performs the same daemon
registration and opens **no** `App` at all, so it takes no repo lock. One command
was holding the repo-wide lock across a call its sibling makes without it.

### The corrected rule

**The exclusive repo lock is held for the duration of the shared-store mutation,
never for the duration of a process — server or CLI.** Any operation that is not
bounded by store work detaches the lifetime lock (`App.DetachLock`) and guards
the mutation itself.

Concretely, `mgit work` and `mgit worktree add` now split provisioning in two:

| Phase | Lock | What runs | Why that scope is correct |
|---|---|---|---|
| **Claim** | held | base resync, task-branch resolve/create, registry insert | every step mutates state shared by all worktrees: the base branch, the refs, the registry |
| **Materialize** | free | marker + branch tree written under the worktree's own path | the path, task and branch are already this process's exclusively (below); the objects read are reachable from the ref the claim created, so a concurrent `gc` cannot collect them |
| **Sandbox** | free | the `mgit-sandboxd` round-trip | touches no repo state whatsoever — the same call `mgit sandbox launch` makes unlocked |

### Why narrowing does not open a race

The lock was not what enforced FR-16's exclusivity rules — the registry was, and
still is. `worktrees` carries `UNIQUE(path)`, `UNIQUE(task_id)` and
`UNIQUE(branch_name)`, and the insert happens **before** anything is written to
disk. So the registry insert is the single point at which a concurrent race is
decided: exactly one claimant wins, every other is refused, and the winner owns
its path/task/branch for the whole unlocked phase. What changed is only that the
refusal is now *named* (`task already bound to a worktree`, `branch checked out
in another worktree`) instead of surfacing a raw SQLite constraint string.

One ordering detail is load-bearing: the worktree's marker is written **before**
its content. A peer's `mgit work` may fingerprint the project while this
worktree is still filling up, and the walk skips any directory carrying its own
`.mgit` — so a half-materialized worktree is never mistaken for project content
and absorbed into the shared base.

### Reentrancy is the hazard to respect

`flock` is per *open file description*, so a second acquire inside the same
process blocks on the first. A component that guards its own critical section
(`WorktreeService.WithLocker`) must therefore be wired **only** by callers that
hold no lock — the CLI commands that have just detached. The MCP and REST
surfaces are already inside a `Guard`, so they wire no locker and the service
runs pass-through, exactly as `Guarder.Guard` treats a nil receiver.

### Timeout

The wait is now `locks.timeout_seconds` from `.mgit/config.json` (default 30s,
capped at 1h), as REQUIREMENTS.md FR-4.7/NFR-3.5 had promised all along while
the code carried a compile-time constant. It is an escape hatch for an operator
on a busy repo, not a remedy: widening the wait converts a wedge into a slower
wedge. The remedy is this amendment's rule.

---

## Second amendment (2026-08-17) — the same rule inside the daemon

**Refs:** MGIT-122

### What went wrong

The first amendment fixed the repo lock and, in passing, named the defect one
layer down without fixing it: `mgit-sandboxd`'s `SandboxService.mu` was held
across `manager.Launch`, a full VM boot.

That mutex has two jobs, and only one of them needs a boot's worth of duration.
It protects the `byTask` registry map — microseconds of work — and it was
*incidentally* serializing boots, which take 16.5 s at four concurrent sandboxes
and 41–59 s at eight on the reference host (`docs/E2E-MATRIX.md`). So while any
one sandbox booted, every registration, exec, status and teardown on the daemon
queued behind it, however healthy their own sandboxes were, and expired on the
client's socket deadline. The agent was shown `the guest stopped answering`
about a guest that was fine, with advice (raise `--memory-mb`) unrelated to the
cause.

`internal/sandboxd/ceiling.go` already had the shape right and said so in its
own doc comment: *"capacity is reserved under the lock, the (possibly slow)
backend launch runs OUTSIDE it… one cold boot never serializes other
operations."* The service above it did not follow.

### The corrected rule

**Same rule as the first amendment, applied to in-process locks: a mutex is
held for the state transition, never for the duration of the work it
authorizes.**

| Phase | Lock | What runs |
|---|---|---|
| **Claim** | held | resolve options, refuse/join a boot already in flight, mark this registration as booting |
| **Boot** | free | `manager.Launch`, egress start, grant replay, port publish |
| **Record** | held | audit event, durable state, mark running, publish the outcome to joiners |

### Why narrowing does not open a race

The lock was never what made a boot exclusive — nothing was, because holding it
made a second boot unreachable rather than refused. The claim is now explicit:
a `bootAttempt` on the registration, set under the lock. Every window that
opens is closed by it rather than tolerated:

- **Two callers, one task.** The second finds the attempt and waits on it. It
  receives the winner's outcome — including its *failure*, deliberately, so N
  callers against a refusing backend produce one launch and not N.
- **A teardown mid-boot.** `Remove`, `Land`, sync and export wait for that
  task's attempt to settle (`awaitBootLocked`, which drops and re-takes the
  lock). Acting through a boot would delete the registration while its VM was
  still being created — a VM alive with nothing tracking it (the MGIT-103
  shape) — and would record `resumed` after `destroyed`, a life continuing
  after it ended, which the fleet soak's invariant I5 refuses. The wait is
  per-task: tearing down one sandbox never waits on another's boot.
- **The TTL sweep.** Skips a registration with a boot in flight; an in-flight
  boot is activity, and the next sweep reaps it if it is still expired.
- **A policy staged mid-boot.** Refused as booted. The launch already took its
  copy of the options, so reporting a stage as successful would be a silent
  fail-open at the moment containment is relied on — the defect MGIT-109 closed
  (`pendingRegLocked`).

`List` and `Status` deliberately do NOT wait: they report what is true now
(`created`, not yet running), which is the honest answer and the one that keeps
an inspection from blocking behind a boot.

### The client deadline, which was the other half

Narrowing the mutex is not sufficient on its own. `internal/sandboxd/client.go`
armed **one** read deadline at dial and never re-armed it, so every request's
budget was spent by whatever happened before it was written. For exec that was
fatal in its own right: the daemon relays a command's output only once the
command has finished, so a deadline on that stream is a cap on how long a build
may take. Measured on a warm, single-VM, otherwise-idle sandbox, `mgit sandbox
exec -- sleep 45` failed at exactly 30 s.

The response budget is now armed when the request goes out, and the exec frame
stream carries no wall clock at all. What bounds it instead is the per-exec
timeout and the sandbox TTL (daemon-enforced), a dead daemon (its socket closes
and the read ends at once), and the caller's context — now wired to the
connection, so cancelling actually cancels. That last part is what makes
removing the deadline safe rather than a trade of a timeout for a hang.

**Known gap, filed not hidden:** while a command runs the daemon sends nothing,
so a client cannot distinguish "the guest is working" from "the daemon is
wedged". Closing that needs a liveness frame on the exec stream (MGIT-133).
