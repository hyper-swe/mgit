# ADR-009: Per-Operation Repo Locking for the Long-Lived Server

**Status:** Accepted
**Date:** 2026-07-03
**Refs:** MGIT-46, MGIT-10 (process lock), ADR-001 (embedded git)

## Context

Every mgit process serializes on one advisory file lock —
`.mgit/locks/mgit.lock`, an exclusive `flock(2)` acquired in `OpenApp` and held
until the process closes its stores (MGIT-10, `internal/store/lock`). For a CLI
command this is exactly right: the lock is held for the command's (short)
lifetime, giving a single-writer guarantee across concurrent `mgit` processes.

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

## Alternatives considered

- **CLI proxies to the running server when the lock is held.** Heavier: it
  couples the CLI to a running server's transport and lifecycle, and does
  nothing when no server is running. Per-operation locking needs no coordination
  and works whether or not a server is up.
- **A separate in-process RW mutex in the server plus the lifetime flock.** Would
  fix in-process races but not the cross-process CLI starvation, which is the
  actual reported bug.
