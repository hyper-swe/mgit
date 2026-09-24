# ADR-016: The Linux release ships the libkrun daemon, with libkrun and libkrunfw bundled

**Status:** Accepted (owner decision, 2026-09-24)
**Date:** 2026-09-24
**Refs:** MGIT-229, hyper-swe/mgit#12, ADR-005, ADR-010, ADR-011, FR-17.15, FR-17.16

## Context

Until this decision the Linux release archives carried the CGO-free
**firecracker** daemon. Two facts made that daemon unable to serve the agent
loop on Linux, and they are independent of each other:

1. **The documented provisioning cannot boot it.** `mgit sandbox base from
   <image>` composes a *directory* base, the shape libkrun boots. firecracker
   boots a kernel and an ext4 rootfs, and nothing on the user path registers
   either. Walked with the released v0.6.8 on a stock `ubuntu-latest` runner
   and on a bare-metal Ubuntu 20.04 host, every first exec failed with
   `firecracker config invalid: failed to stat kernel image path, ""`. The
   documented firecracker alternative, `sandbox image install`, fetches from
   release assets that were never published (HTTP 404).
2. **A booted firecracker guest would still refuse the loop's verbs.**
   firecracker delivers the worktree as a launch-time ext4 image, so it
   refuses `sandbox sync` and `sandbox export` by design (ADR-011). An agent
   loop edits the host worktree between rounds and exports build artifacts
   back out; that loop has no firecracker shape.

Linux **libkrun** (`-tags libkrun`) serves both verbs and has been
live-validated in CI on every push (the Linux libkrun column: boot, exec,
sync, export, the SEC-03 hostile-guest battery, egress and live policy). It
was not what the release shipped, because libkrun and libkrunfw are native
libraries, and no Ubuntu release packages either of them.

## Decision

The Linux release archives (amd64 and arm64) carry **mgit-sandboxd linked
against libkrun**, with **libkrun.so.1 and libkrunfw.so.5 beside it in
`lib/`**. A stock Linux host needs only `/dev/kvm`.

How it is built and shipped:

- **One assembler**, `scripts/release/build-linux-sandboxd.sh`, builds libkrun
  and libkrunfw from the pinned sources (`scripts/sandbox-image/pins.env`: the
  versions Homebrew ships to macOS, so both platforms run the same VMM) inside
  **ubuntu:20.04**, so the **glibc floor is 2.31**. It links the daemon
  against them and verifies its own output before anything is uploaded: run
  paths, the networking symbols, the glibc floor, a clean-environment load
  with the build prefix hidden, and a negative control (with libkrunfw
  removed, the daemon must say so).
- **Run paths are DT_RPATH, not DT_RUNPATH.** libkrun `dlopen()`s libkrunfw
  by leaf name from its own code. libkrun.so carries DT_RPATH `$ORIGIN`, so
  that search finds the bundled libkrunfw *before* `LD_LIBRARY_PATH`, which the
  VM child's environment extends with the usual system prefixes. With
  DT_RUNPATH, a system libkrunfw of another version would win. The daemon's
  DT_RPATH covers the extracted archive (`$ORIGIN/lib`) and install.sh's
  layout (`$ORIGIN/../lib/mgit`).
- **One build job.** e2e.yml's `linux-sandboxd` job runs the assembler for
  both architectures on every pull request. The release runs e2e.yml as its
  gate, and it ships the artifacts that job uploaded in the same run: the
  bytes the Linux user-path leg just booted a guest with.
- **goreleaser takes the daemon through a stand-in build tool**
  (`scripts/release/gobinary-prebuilt.sh`). The release runs on a macOS
  runner, and open-source goreleaser can neither cross-compile a cgo Linux
  binary nor accept a prebuilt one. The stand-in copies the prebuilt daemon,
  its `lib/` and its license texts. It verifies them against the assembler's
  manifest before and after the copy, and it refuses a daemon whose version,
  commit or date stamp differs from the one goreleaser stamps into `mgit`. It
  never falls back to compiling, because a CGO-free compile would silently
  ship the firecracker daemon. Every build now stamps `{{.CommitDate}}`
  instead of the build time, so a separately built daemon can carry the
  identical stamp. The user-path leg runs goreleaser non-snapshot against a
  local tag, so that check runs on every pull request.
- **The corresponding source ships with the binaries.** libkrunfw compiles a
  Linux kernel (6.12.x, GPL-2.0-only) into itself, and its glue is
  LGPL-2.1-only. Every release that bundles it publishes, as release assets
  covered by the signed checksums: the pinned kernel tarball, byte-identical to
  kernel.org's; libkrunfw's tree at its tag (the patches, configuration and
  build scripts applied to that kernel); and libkrun's tree at its tag. Each
  Linux archive carries `THIRD_PARTY/` with the license texts and a
  `SOURCES.txt` that names those assets. The release notes say so.

## Consequences

- **This reverses ADR-010's distribution stance for Linux.** ADR-010 kept
  libkrun and libkrunfw a user-installed prerequisite, so that mgit
  redistributed no kernel binary. On Linux there is no installer to delegate
  to, and a first-hour user would have to compile a kernel. mgit now
  redistributes both libraries on Linux and carries the GPL-2.0 obligation for
  the bundled kernel, discharged by publishing its corresponding source with
  every such release. macOS is unchanged: libkrun still comes from Homebrew.
  (Engineering-grade compliance reasoning, not legal advice.)
- **A glibc floor.** The bundle needs glibc 2.31 or newer (Ubuntu 20.04,
  Debian 11 and later). The assembler refuses a build that needs more.
- **linux_arm64 is build-verified, not boot-verified.** No hosted CI runner
  exposes KVM on arm64: `/dev/kvm` is absent on `ubuntu-24.04-arm`. Its daemon
  is built and load-checked, including the libkrunfw resolution, but no guest
  boots in CI. MGIT-230.5 owns closing that gap or stating it.
- **Bigger archives and a longer CI.** Each Linux archive grows by the two
  libraries, and every pull request builds libkrun and libkrunfw for both
  architectures (the kernel compile dominates).
- **firecracker stays in the tree.** A plain `go build` or `go install` of
  mgit-sandboxd on Linux still produces the CGO-free firecracker daemon, and
  its sync/export refusal stays as designed. Its retirement is MGIT-61.13's
  decision, not this one.
- **doctor can tell a daemon that loads from one that can boot.**
  `mgit-sandboxd --vmm` reports the linked VMM and where its libraries
  resolved, asked in a child with the VM child's own environment. The
  `daemon/vmm` row relays it, because libkrun loads libkrunfw lazily: a
  missing libkrunfw passes `daemon/loads` and fails every launch.

## Alternatives considered

- **Users supply libkrun.** Link libkrun but do not bundle it: users build it
  or find a package. No Ubuntu release packages it, so the first hour of every
  Linux user becomes a kernel build. Rejected.
- **Teach firecracker kernel+rootfs composition, sync and export.** The
  largest option, and it cuts against consolidating on one cross-platform VMM
  (ADR-010). Rejected.
