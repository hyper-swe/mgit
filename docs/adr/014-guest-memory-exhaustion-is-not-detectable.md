# ADR-014: guest memory exhaustion is not reliably detectable from the host, so the cap is made visible instead

**Status:** Accepted
**Date:** 2026-08-12
**Refs:** MGIT-95, R-H212, NFR-17.5, FR-17.26, FR-17.11

## Context

A customer's production build peaked at 2.10 GB RSS. mgit's guest default is
2 vCPU / 2048 MB, which leaves ~1.94 GB usable, so the build aborted in-guest
(V8 old-gen exhaustion, exit 134) and succeeded on the host. The dev agent
responded by **reshaping the production bundler configuration** to fit — a
posture-dependent fact nearly became permanent product code in the customer's
repository, caught only by human review.

The agent behaved reasonably on what it could observe: a build that dies, and
no way to see why. MGIT-95 gave the workload a way to declare its own size
(`--memory-mb` and friends, bounded by host policy). But declarable memory
alone does not fix the incident: the **next** build to hit a ceiling still
fails opaquely, and the next agent still "fixes" the bundler.

The obvious remedy is to detect memory exhaustion and say so. We investigated
whether that is possible.

## What we found

Memory exhaustion in the guest has (at least) two shapes, and neither yields a
reliable host-side signal.

**1. The runtime aborts itself before the kernel is involved — the customer's
case.** Node/V8 sizes its old-generation heap from the memory it can see, so a
small guest produces `JavaScript heap out of memory` and `abort()`. The kernel
never runs its OOM killer, so there is no kernel event to detect — no dmesg
line, no cgroup counter, nothing. The only artifact is a SIGABRT, which
reaches the host as exit 134 *if* the process died under the shell mgit wraps
the command in, and as `-1` if it was the direct child (Go's
`ProcessState.ExitCode` does not encode signals). A `kill -9` from a test
script produces the same statuses, so the exit code alone proves nothing.

**2. The kernel's OOM killer does fire — and takes the reporter with it.**
Reproduced live on macOS/libkrun (MGIT-95, 2026-08-12) by filling a tmpfs past
a 2048 MB guest's RAM. The host did not receive an exit code at all: the exec
channel dropped mid-command (`read frame: EOF`), and every subsequent exec
failed with `guest vsock not ready ... connect: connection refused`. The guest
supervisor is PID 1 in the guest; when the kernel starts killing, it is a
candidate, and reading the guest's own `dmesg` afterwards is impossible
because the guest is gone.

So a guest-side probe (`/dev/kmsg`, an OOM annotation on the result frame)
would cover only the case where the guest survives — and would additionally
require every already-built guest image to be rebuilt before any deployed
sandbox benefited. The case that motivated the ticket would still report
nothing, because in that case the kernel has nothing to report.

## Decision

**mgit does not claim to detect guest memory exhaustion. It makes the ceiling
visible instead, at every point where an agent would otherwise infer it.**

1. **Before the fact.** The effective caps are resolved at registration, carried
   on `SandboxInfo`, printed by `mgit sandbox status`, and stated in the
   generated CLAUDE.md block — together with the explicit instruction not to
   reshape the project to fit the guest.
2. **At the point of failure.** `mgit run` and `mgit sandbox exec` print the cap
   in force when a command dies by a signal (134 / 137 / -1) **or** when the
   guest stops answering mid-command. Both messages are phrased as context, not
   as a diagnosis: they state what mgit knows for certain (the cap) and what to
   do if the workload needs more.
3. **When the size is wrong.** A launch above the host policy's per-sandbox
   maximum is refused naming the limit, never clamped — so a caller can never
   quietly receive less than it asked for and conclude memory was ruled out.

## Consequences

- An ordinary `kill -9` in a guest also prints the advisory. Accepted: the
  message costs a reader five lines and never asserts the cause, while its
  absence cost a customer a modified production build.
- A guest killed by its own kernel still leaves the sandbox unusable and its
  recorded state stale (`running`). Reaping it is out of scope here and is
  tracked separately.
- If a future guest agent can survive an OOM kill (a supervisor with
  `oom_score_adj = -1000`, reading `/dev/kmsg` on the child's death), an exact
  "the kernel killed your process at N MB" becomes possible for shape 2. This
  ADR does not preclude that; it records that shape 1 — the one we actually
  saw — remains undetectable in principle, so the visible cap is the load-
  bearing mechanism either way.
