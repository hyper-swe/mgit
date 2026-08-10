# ADR-011: Worktree sync, live policy revoke, and artifact export

**Status:** accepted; MGIT-71 (sync) and MGIT-73 (export) implemented, MGIT-72 (revoke) implemented, MGIT-76 (explicit sync verb) outstanding
**Date:** 2026-08-09
**Refs:** MGIT-71, MGIT-72, MGIT-73, MGIT-76, SEC-03, SEC-04, SEC-05, SEC-10, ADR-005, ADR-010

These three capability gaps are one design conversation. MGIT-71 moves files
host→guest, MGIT-73 moves them guest→host, and MGIT-72 changes policy on a
live sandbox. A decision in any one constrains the others, so they are settled
together here.

## The constraint that is not up for negotiation

The guest's worktree is a **staged copy**, not a live view of the user's
worktree, and it stays that way. The reasoning is already recorded in
`vzf/hypervisor_darwin.go` ("DESIGN TRADE-OFF (copy vs. live)"): a live
virtiofs share cannot host-side exclude an in-worktree `.git`/`.mgit`, cannot
rebind `.mgit` to the sandbox-local store, and cannot reject an escaping
symlink before the guest follows it. virtiofs offers no per-entry deny and no
symlink-resolution boundary, so **a live share cannot fail closed**.

Therefore: **every byte crossing the boundary in either direction goes through
`internal/sandboxd/staging`'s invariants**, which are enforced host-side
before the guest can act on them. This rules out a bidirectional mount and
rules in an explicit, audited, host-driven transfer.

## The delivery asymmetry the tickets do not mention

The backends deliver the worktree differently, and it changes what "apply" can
mean:

| Backend | Delivery | Can the host update it while the guest runs? |
|---|---|---|
| libkrun (macOS default), vzf | virtiofs share of the **staged host directory** | **Yes** — the staging dir *is* the guest's worktree |
| firecracker (Linux) | an **ext4 image** built with `mke2fs -d` at launch | **No** — the guest has it mounted; host writes would corrupt it |

So a single "re-stage the directory" mechanism works on two backends and is
impossible on the third. The resolution keeps one semantics without pretending
the mechanics are the same:

> **The host decides WHAT to apply; the backend decides HOW to apply it.**

The staging build, the delivery manifest, the conflict detection and the
collision policy are shared, backend-independent code — the part that carries
the security properties and the user-visible behaviour. Only the final apply
step is per-backend: a host-side directory write for virtiofs, a
guest-mediated push over the control plane for firecracker. This mirrors how
worktree *delivery* is already split, and keeps the promise ADR-005 made of
one delivery semantics across backends.

## MGIT-71 — the collision policy (the hard part)

Sync is a **host-driven, all-or-nothing update of paths the host previously
delivered**. Every path in the guest's worktree falls into one of three
classes, and the class decides its fate:

1. **Host-delivered, guest-untouched** — updated (or deleted) to match the
   host. This is the capability being added.
2. **Host-delivered, guest-modified** — **CONFLICT**. The sync is refused
   entirely, nothing is applied, and the conflicting paths are named with a
   remedy. `--force` overwrites them, and every overwritten path is audited.
3. **Guest-created, never host-delivered** — **never touched, never deleted.**

Class 3 is not a detail. A naive "make the guest tree match the host tree"
would delete `node_modules` and every build cache on each sync — destroying
exactly the work MGIT-73 exists to preserve, on every round of the loop.

Conflict detection uses a per-sandbox **delivery manifest** (relative path →
SHA-256 + mode) written at launch and updated on each sync. A path conflicts
when its current guest content differs from what the manifest says was
delivered *and* the host copy has also changed. A guest that misreports its
own hashes only harms itself: it cannot cause a host write, and it cannot
widen what the host sends.

**Why refuse rather than pick a winner.** Host-wins silently destroys
un-landed guest work; guest-wins silently keeps testing stale code, which is
the bug being fixed. Both are quiet wrong answers. Refusing is
honest-blocked-over-dishonest-done, the same call the land airlock makes.

**Atomicity.** Sync holds the sandbox's exec lock, so no exec observes a
half-applied tree. A sync that fails partway leaves the manifest unchanged, so
the next attempt re-derives the same work.

Files are written **in place**, not by the usual write-then-rename. That is
deliberate and was forced by measurement: the destination is a virtiofs share
the guest has mounted, and a host-side rename swaps in a new inode underneath
the guest's cached dentry. On a real libkrun VM a renamed file read as
**ENOENT inside the guest** — while the host tree was verifiably correct — and
a host deletion stayed visible. Truncating and rewriting keeps the inode and
the guest observes the new content. The ordering guarantee rename would have
provided comes from the exec lock instead. This is a property of a live shared
filesystem that no amount of host-side correctness substitutes for, and it is
the reason this feature had to be validated on a real VM rather than in a
unit test.

**When it runs.** Automatically before an exec when the host worktree has
changed (cheap manifest comparison; unchanged means no work at all), plus an
explicit `mgit sandbox sync` for control. An exec blocked by a conflict fails
closed naming the conflicting paths — an exec that silently runs against stale
code is precisely the reported defect.

The explicit verb is not a convenience wrapper around the automatic one; it
exists because two things are unobtainable without it. It **re-stages without
running anything in the guest** — the integrating lane's workaround was a
probing no-op exec every round, which is this verb spelled awkwardly at the
cost of a guest process per round. And `--dry-run` **returns the collision
classification without touching the guest**, so "which paths diverged" stops
being discoverable only by attempting work and being refused. `--force` carries
the pre-exec semantics unchanged: it overwrites, and every destroyed path is
reported and audited.

It is a CALLER, not a second mechanism. The verb, the automatic pre-exec stage
and a launch all run the same `internal/sandboxd/staging` build and the same
collision policy, so a sync can never deliver what either of the others would
have refused — and that single-path property is the whole security argument for
staging over a live mount. A dry run stops after the classification, before any
write, and leaves the delivery manifest where it was; a query that advanced the
baseline would make the next real sync believe the work was already delivered.
It is never recorded as a sync in the audit trail either: an audit that cannot
tell a query from a delivery is worse than none.

On a backend that delivers the worktree as a launch-time image (firecracker),
the verb **fails closed naming the limitation** rather than reporting a sync
that did not happen — the host cannot write into an ext4 image the guest has
mounted, and a verb that claims to have run is how stale code gets executed.
Reporting the refusal is the honest answer; re-launching is the remedy.
(`MGIT-76` shipped the verb; `MGIT-71` shipped the mechanism.)

## MGIT-72 — established flows: kill, not drain

**Revoke kills established flows by default.** `--drain` lets them finish and
is opt-in.

A caller who revokes package-registry egress and then runs untrusted code
expects the grant to be *gone*. A draining connection is exactly the
exfiltration channel they just revoked — a long-lived one survives arbitrarily
long, so "drain" can mean "never" in the presence of a hostile guest. The
weaker behaviour is available, but it must be asked for by name.

Mutation is atomic (a flow is authorized against the old policy or the new
one, never a mixture), host-only (SEC-05 — no control-plane route reaches this
from the guest), and appended to the audit trail with task binding and the
change detail.

## MGIT-73 — export, and what "land is the only bridge" means afterwards

Export is the land airlock's shape applied to files: **the host names both the
guest source and the host destination.** A guest-chosen destination is a
host-filesystem write primitive and is never accepted.

Every entry is validated host-side **before any host write**: relative paths
only, no `..`, no absolute paths, no symlink or hardlink leaving the exported
subtree, with `staging`'s existing checks reused rather than reimplemented.
Collisions are **refused** by default. Size and file-count limits bound the
transfer (T7). Every export is audited with task binding, path and byte count.

**Provenance: yes.** An exported tree carries a sidecar manifest naming the
sandbox, the task, the pinned base-image digest and per-file hashes, following
the MGIT-61.15 attestation pattern. A `node_modules` tree in a host cache with
no record of which sandbox produced it is a supply-chain artifact of unknown
origin.

**Restating the invariant.** `TestE2E_*_HostileGuest_LandIsOnlyBridge` asserts
something precise and still true: no guest activity changes the host's
**shared git store** without land. Export does not weaken that — it is a
second bridge for **files into a host-named destination**, gated by a
host-initiated verb, and it never touches the git store. The tests are
re-stated to say exactly that rather than deleted, and export gets its own
hostile-guest coverage proving the guest cannot write outside the destination
the host named.

### What shipped (2026-08-10, MGIT-73)

The design above is implemented in `internal/sandboxd/artifactexport` (the
host-side engine), `microvm.Manager.ExportArtifact` (the backend seam),
`SandboxService.ExportArtifact` (task resolution + audit), the `KindExport`
control verb, `mgit sandbox export --task <id> <guest-path> <host-path>`, and
the `mgit_sandbox_export` MCP tool. The decisions the ticket asked to be
recorded, as built:

- **No control-plane hop, per the backend-asymmetry finding.** libkrun and vzf
  export by READING the staged host directory; the guest is never asked and
  cannot interpose. `vmctl` is untouched.
- **firecracker fails CLOSED** with `model.ErrArtifactExportUnsupported`,
  naming the limitation (a launch-time ext4 image has no host directory to read
  from). The guest-mediated stream it would need is deliberately **not** in v1 —
  the same call MGIT-71 made in the opposite direction. Shipping libkrun/vzf
  first is the scope cut; the refusal, not a silent downgrade, is the contract.
- **Collision policy: REFUSE.** The destination and its sidecar must both be
  absent; an export never overwrites, merges into or deletes host state, and the
  destination's parent directory must already exist. Documented in the verb's
  `--help`, the MCP tool description, and this ADR.
- **Limits: 4 GiB and 200,000 entries by default** (`artifactexport.Limits`),
  enforced during planning — before any write — and re-enforced against the
  bytes actually read, so a file that grows between plan and copy is refused
  rather than admitted.
- **Whole-worktree and private-store exports are refused.** `.` and
  `<worktree>/.mgit` are rejected by name: committed objects cross only through
  land, so export cannot become a second, unverified route for them.
- **Provenance: yes, as a sidecar.** `<host-path>.mgit-export.json` carries the
  schema version, sandbox ID, task, backend, pinned base-image digest, per-file
  SHA-256 + mode, and a tree hash over the canonical manifest. A sidecar rather
  than a file inside the tree, so the exported artifact is byte-for-byte what
  the guest built.
- **Audit: `artifact_exported`**, an audit-only sandbox event (no state change)
  carrying task binding, both paths, file and byte counts and the tree hash. An
  export that cannot be audited is **undone** — a file on the host with no
  record defeats the trail.

### File modes: measured, then resolved (MGIT-81)

MGIT-73 shipped with a measured limitation: on macOS/libkrun a file the guest
wrote `0755` read as `0600` on the host share, and the export — which
deliberately reproduces only modes it has observed — carried the `0600`
outward, so an exported `node_modules` tree's `.bin` scripts arrived
non-executable. MGIT-81 measured *where* the bits went and found the mode was
observable host-side after all.

**The measurements** (2026-08-10, Apple Silicon; the numbers are reproduced by
`TestE2E_Libkrun_RealVM_ModeFidelity_HostCanObserveTheModeTheGuestSet` and
`TestE2E_VZF_ModeFidelity_TheShareCarriesTheModeAndExportReproducesIt`, both
real-VM):

| what the guest did | guest on tmpfs (control) | guest on the share | host `lstat` | host share record |
|---|---|---|---|---|
| libkrun 1.19.4, write `0755` | `0755` | `0755` | **`0600`** | `0:0:0100755` |
| libkrun 1.19.4, write `0644` | `0644` | `0644` | **`0600`** | `0:0:0100644` |
| libkrun 1.19.4, `chmod 0755` | `0755` | `0755` | **`0600`** | `0:0:0100755` |
| libkrun 1.19.4, `mkdir 0755` | `0755` | `0755` | **`0700`** | `0:0:0755` |
| vzf (Virtualization.framework), write/chmod `0755` | `0755` | `0755` | `0755` | *(none)* |

The guest's umask was `0022` on both backends and the tmpfs control arm kept
every mode, so **neither the guest nor the workload loses anything**: the
mapping is libkrun's macOS filesystem device, which gives guest-created inodes
placeholder permission bits (`0600` files, `0700` directories) and records the
real `st_mode` in the `user.containers.override_stat` extended attribute — the
containers-ecosystem convention. Host-created files in the same staged tree
carry no such record. vzf carries modes in the permission bits and writes no
record at all.

**The decision.** The export reads the share's record when there is one and the
host's own permission bits when there is not, and reproduces that mode with an
explicit `chmod` (an `O_CREATE` mode is masked by the exporting process's
umask, which would have shaved an observed `0755` to `0700` under a `0077`
daemon). This keeps the MGIT-73 property intact: **the guest does not
participate.** The record is written by the backend's *host-side* filesystem
device as it services the guest's `create`/`chmod`, exactly as the permission
bits are on a backend that carries them; the guest is never asked, and cannot
tell an export happened. No mode is invented — the only question resolved was
which of two real host-side observations to trust.

**Bounds on trusting the record.** Only permission bits are taken, masked to
`0777`: uid/gid are ignored (an exported artifact belongs to the exporting
user), setuid/setgid/sticky are dropped as they always were, and the record
never decides what a file *is* — regular, symlink or directory comes from the
host stat alone, so a hostile record cannot talk the export into treating a
symlink as a regular file and sidestepping the escape checks. A malformed
record is ignored rather than half-read, and the widest a well-formed one can
ask for is `0777`, which the guest could equally have reached by an honest
`chmod` on a backend that carries modes.

**Attribution in the sidecar.** An entry whose mode came from the share record
is marked `"mode_source": "share-record"`; a plain host stat is the default and
stays implicit. It is excluded from the tree hash, so the same tree exported
from libkrun and from vzf hashes identically. Exported **directories** are
created `0750` whatever the guest set — an export widens nothing on the host
beyond the user who asked for it.

**Still true:** a backend whose share neither carries modes nor records them
would export the placeholder bits, and the sidecar would say so by *not*
claiming a share record. That is the honest failure, and it is what the two
real-VM fidelity tests exist to catch.

## The test rule, applied

Every deny assertion needs a matching allow assertion, and a denial must be
distinguishable by reason from a broken path:

- **sync** — prove the guest sees *new* content, not merely that stale content
  is gone; and that a conflict is refused with its paths named. The explicit
  verb needs a **positive control on the same running VM**: a sync that really
  delivers, so a refusal is distinguishable from a broken path.
- **revoke** — prove allow-with-real-bytes *then* deny-by-reason, both
  asserted, on one running sandbox.
- **export** — prove a real tree lands intact *and* that a malicious one is
  refused before any host write.
