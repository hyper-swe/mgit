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
   do if the workload needs more. The advisory is **gated on the observed
   phase** — see the amendment below.
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

## Amendment (MGIT-104, 2026-08-12): the advisory is gated on the observed phase

The first version of decision point 2 attached the cap advisory to **every**
unreachable guest. Found live on macOS/libkrun: a daemon without
`com.apple.security.hypervisor` cannot create a VM at all, so the launch failed
closed and printed the guest console tail containing
`krun_start_enter: libkrun error -22` — and mgit then appended "the guest
stopped answering mid-command" plus a cap advisory pointing at `--memory-mb`.
Two inaccuracies at once: a guest that never started cannot have exhausted its
memory, and the same message had just said the guest never answered.

This is the MGIT-95 incident inverted. There, an agent inferred a cause from a
failure with no visible ceiling. Here, a failure with an already-printed cause
gets annotated with a memory hint the reader may act on. **A diagnostic that
points at the wrong fix is worse than none, because it is acted upon** — and it
spends the advisory's credibility in the cases where it is right.

So mgit now names the phase it actually observed, and prints exactly one
diagnosis:

| Phase | Evidence | What is printed |
|---|---|---|
| never started | `ErrGuestNotServing` in the failure, or a VM-start marker (`krun_vm_failed` / `krun_vm_bootfail` / `krun_start_enter`) in the console tail | the start failure verbatim, its remedy where mgit can name one, and an explicit statement that the memory cap is **not** implicated. No cap advisory. |
| never started, cause unidentified | `ErrGuestNotServing`, empty or unrecognized console | the phase, and a pointer to the console output and `mgit sandbox status`. No invented cause, no cap advisory. |
| lost while serving | a dropped exec channel / refused dial with no launch failure | the MGIT-95 cap advisory, unchanged. |
| signal exit (134 / 137 / -1) | the guest exit code | the MGIT-95 cap advisory, unchanged. |

The advisory itself — including its "do not reshape the build to fit the
sandbox" line — is **not** weakened. Only its trigger changed.

On darwin, a VM-start failure additionally probes the local `mgit-sandboxd`
with `codesign --display --entitlements -` (the same check
`scripts/e2e/sandbox_posture.sh` gates on) and, when the hypervisor entitlement
is absent, names it with the signing command. "Cannot tell" is a first-class
verdict: an unprobeable daemon is never reported as unsigned.

## Amendment (MGIT-118, 2026-08-19): no phase is reached by elimination

MGIT-104's amendment gated the advisory on evidence but left "lost while
serving" as the branch every *unrecognized* failure fell into — so the phase
carrying the advisory was also the classifier's answer for "I do not know". Four
causes then had to be carved out of it one at a time, each after a user was told
their guest had run out of memory:

| Cause | Ticket | What it actually was |
|---|---|---|
| VM never booted | MGIT-104 | an unsigned daemon that could not create a VM |
| fleet ceiling refusal | MGIT-118 | the HOST out of capacity; no VM attempted |
| daemon stall | MGIT-133 | `mgit-sandboxd` stopped beating; the guest may still be working |
| wire-version mismatch | MGIT-136 | two host binaries that never transacted |

A default that asserts a specific diagnosis is wrong every time reality adds a
case; a default that reports the evidence and names no cause is only ever
incomplete. So the classifier is inverted: **every** phase — `lost while
serving` included — is now reached only by evidence that positively supports it,
and anything else is `unidentified`.

| Phase | Evidence | What is printed |
|---|---|---|
| admission refused | `ErrSandboxCeilingExceeded`, by `errors.Is` or by text | that no VM was started, that this sandbox's size is not the problem and raising it makes the refusal *more* likely, and that the fix is host capacity. No cap advisory. |
| lost while serving | a transport failure only a guest that HAD answered can produce: `EOF`, `connection reset`, `broken pipe`, `use of closed network connection`, `connection refused` | the MGIT-95 cap advisory, unchanged. |
| unidentified (default) | none of the above | the phase is not named, no cause is claimed, and the reader is pointed at the error itself, `mgit sandbox status` and `mgit sandbox list`. No cap advisory. |

`unidentified` is deliberately the **zero value** of the phase type, so a phase
nobody assigned cannot diagnose anything either, and a phase added without a
renderer degrades to the honest report rather than to whatever sat in the
default branch.

What this costs is real and accepted: a genuine mid-command loss whose transport
error uses wording not in the marker list is reported as unidentified, and the
reader loses the cap advisory on a failure where it would have been right. That
is the cheaper error. A missing diagnosis costs a reader one `mgit sandbox
status`; a confident wrong one — measured twice — costs an afternoon of
shrinking a build that was never too large. `mgit sandbox exec`'s exit-code path
(a signal death, 137/134/-1) is unaffected and still names the cap directly.

The advisory is still not weakened. Only the set of things that can reach it is.
