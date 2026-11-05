# ADR-017: Windows sandbox through a WSL2 distribution

**Status:** Proposed
**Date:** 2026-11-03
**Refs:** ADR-005, ADR-006, ADR-010, ADR-011, ADR-016, FR-17.1, FR-17.3, FR-17.15, FR-17.16, FR-17.27

## Summary

mgit has no sandbox on Windows. This document proposes one: a dedicated WSL2
distribution hosts the Linux `mgit-sandboxd` that ships today, and that daemon
boots one microVM per task inside the distribution, with libkrun on the
distribution's `/dev/kvm`. The Windows `mgit` talks to the daemon through
`wsl.exe`.

The design needs nested virtualization. Without it, the distribution would be
the only boundary, and that is not a sandbox (see
[Why not a distribution per task](#why-not-a-distribution-per-task)). In that
case mgit refuses and says why.

This ADR revisits one line of ADR-006, which rejected WSL2. The reason is
below. Nothing here is built yet. The only code in this change is the refusal
message that Windows users now see.

## Today

### What exists

The sandbox has three layers.

| Layer | Where | What it does |
|---|---|---|
| CLI | `cmd/mgit/sandbox*.go` | Finds the repository, starts or reaches the daemon, sends control requests. |
| Daemon | `cmd/mgit-sandboxd`, `internal/sandboxd` | Serves the control protocol on a unix socket. Owns the registry, the egress policy, land verification and attestation. |
| Backend | `internal/sandboxd/backend/*` | Creates and runs the guests. |

All backends satisfy `model.SandboxManager`:

```go
Launch(ctx, SandboxLaunchOptions) (*SandboxInfo, error)
List(ctx) ([]SandboxInfo, error)
Exec(ctx, id, ExecRequest) (*ExecResult, error)
Stop(ctx, id, force bool) error
Remove(ctx, id, force bool) error
Resolve(ctx, id) (*SandboxInfo, error)
```

The VM backends share one implementation, `microvm.Manager`. A platform
supplies the parts that differ:

- `Hypervisor.CreateVM(VMConfig) (VM, error)`, where `VM` has `Start` and
  `Stop`. `VMConfig` is the isolation contract: CPUs, memory, image paths, the
  worktree path and mount tag, network mode and allowlist, vsock on or off,
  published ports.
- `GuestDialer.DialGuest(ctx, id) (net.Conn, error)`, the control channel to
  the guest. It is vsock on Linux and macOS.
- Optional parts: `GuestPortDialer`, `PeerBinder`, `PeerIdentifier`,
  `NotifyListenerProvider`, `NotifyRegistrar`, `ConsoleTailer`.

The daemon picks a backend in `sandboxd.SelectBackend`. A build-tagged
`newHypervisorBackend` in `cmd/mgit-sandboxd/platform_backend_*.go` returns the
manager and the land dialer for the platform.

### Per platform

| Host | Backend | Guest | Worktree delivery |
|---|---|---|---|
| Linux | libkrun (release archive), firecracker (`go install`) | Linux | libkrun: virtiofs, with `sync` and `export`. firecracker: ext4 image built at launch (ADR-011). |
| macOS arm64 | libkrun on Hypervisor.framework | Linux | virtiofs |
| macOS, `-tags vzf` | Virtualization.framework | Linux | virtiofs. Not shipped. |
| Windows | none | none | none |
| any | container, only with `--backend container --acknowledge-reduced-isolation` | host kernel | bind mount. Reduced isolation, audited. |

On a platform with no backend, `sandboxd.NewUnavailableManager` refuses every
launch with `ErrSandboxBackendUnavailable`. `internal/sandboxd/backend/hyperv`
is a placeholder for the Windows container backend that ADR-006 chose. Epic
MGIT-12 tracks that work. It is not started.

### What Windows users saw before this change

The Windows `mgit` builds, but `mgit sandbox ...` and `mgit run` reached the
connector, which looked for a repository and then for `mgit-sandboxd`. The
Windows archive has no daemon. The user got "mgit-sandboxd binary not found",
which reads as an install problem. It is not one.

## Decision

Proposed, pending the spike in [Plan](#plan).

1. A Windows host runs the sandbox daemon inside a WSL2 distribution that mgit
   creates and owns, named `mgit-sandbox`.
2. The distribution runs the Linux `mgit-sandboxd` with the libkrun backend,
   built exactly as ADR-016 describes. One microVM per task boots inside it.
3. The Windows `mgit` reaches the daemon through `wsl.exe`. It starts no
   Windows daemon and opens no network port.
4. Without `/dev/kvm` inside the distribution, mgit refuses to launch. It does
   not fall back to running the work in the distribution.

This keeps FR-17.1 (one VM per task) and reuses the daemon, the guest agent,
the egress gateway, land verification and attestation unchanged. The new code
is on the Windows side: a `wsl.exe` driver, a connector, a path mapping and
doctor rows.

### How this relates to ADR-006

ADR-006 chose host-matching guests: a Windows host gets a Windows guest. It
rejected WSL2 for one reason that still holds: all WSL2 distributions share one
utility VM and one Linux kernel, so a distribution per task is not a VM per
task.

This design agrees with that reason. It does not use a distribution as the
isolation unit. It uses the distribution as a Linux host for the existing
microVM backend. Isolation comes from the nested microVM, as on Linux.

The costs against ADR-006:

- The guest is Linux, not Windows. Native Windows toolchains (MSVC, .NET
  Framework, PowerShell scripts) do not run in it. This is the same exception
  ADR-006 already grants macOS. It serves developers whose build runs on Linux
  or who already use WSL2. It does not serve developers whose build is
  Windows-only. The WCOW design in ADR-006 remains the answer for them.
- Nested virtualization narrows the set of machines (see
  [Virtualization](#virtualization)).

The two designs do not exclude each other. If both are built, the Windows
backend choice becomes a setting. This ADR does not decide that.

## Design

### Hosting the guest

mgit creates one distribution per Windows user, `mgit-sandbox`. It is not the
user's own distribution, and mgit never touches the user's others.

**Root file system.** mgit builds the distribution from a pinned archive that
holds a minimal Linux user space, `mgit-sandboxd`, and `lib/` with libkrun and
libkrunfw. The pins are the ones the Linux release uses (ADR-016). The archive
is checked against a digest before import, as guest bases are today (FR-17.17).

**Distribution configuration.** Written to `/etc/wsl.conf` in the distribution
at import time:

```ini
[automount]
enabled = false          # no /mnt/c; the distribution cannot read Windows drives
[interop]
enabled = false          # cannot start Windows programs
appendWindowsPath = false
[boot]
systemd = false          # mgit-sandboxd is started on demand, not by an init system
[user]
default = mgit           # unprivileged; the daemon and the VMM do not run as root
```

`wsl.conf` is per distribution. `.wslconfig` is global to all of the user's
distributions and belongs to the user. mgit does not edit it. It reads it, and
`doctor` reports the values that matter:

- `nestedVirtualization` must not be `false`.
- `networkingMode` should be `nat`. `mirrored` shares the loopback interface
  with Windows. See [Security boundaries](#security-boundaries).
- `kernel` set to a custom kernel is reported, because the nested KVM comes
  from that kernel.

**State.** The daemon's work directory, the registry, the audit index and the
staged worktrees live on the distribution's ext4 disk, never under `/mnt/c`.
SQLite in WAL mode needs shared memory that the Windows file-system bridge does
not provide reliably, and the project's store rules require WAL.

### Windows host side

**Paths.** The Windows worktree is `C:\Users\me\proj\wt`. The guest must see a
stable absolute path (FR-17.3). A drive-letter path cannot be a guest path.

Proposal: the guest sees `/mnt/c/Users/me/proj/wt`, the path WSL itself would
use. The mapping is one-to-one and documented. No Windows drive is mounted
there: mgit stages a copy on the distribution's ext4 disk and shares that into
the guest at the mapped path. FR-17.3 says "identical absolute path". This
needs an amendment that says "a defined, reversible mapping on Windows". That
amendment is an open question.

Edge cases the mapping must decide, each with a test:

- Paths over 260 characters and `\\?\` prefixes.
- UNC and mapped network drives: refuse.
- Case-insensitive collisions: two guest files that differ only in case cannot
  both exist on NTFS.
- Names Windows forbids (`CON`, `NUL`, trailing dots or spaces, `:`).
- Symlinks, which need a privilege on Windows.
- The execute bit, which NTFS does not store.
- Line endings. mgit does not translate them. A script with CRLF line endings
  fails in the guest, and `doctor` should say so when it sees one.

The last four also apply at land. The guest can create a file that the host
cannot write. Land must refuse such a commit with the path and the reason. It
must not drop the file.

**Process control.** The CLI runs the daemon client as a child of `wsl.exe`:

```
wsl.exe -d mgit-sandbox --user mgit --exec /opt/mgit/bin/mgit-sandboxd --bridge
```

`--bridge` connects to the daemon's unix socket inside the distribution and
copies bytes between that socket and its own stdin and stdout. The control
protocol is already framed over a `net.Conn`, so a connection backed by the
child's pipes carries it unchanged. `sandboxd.Client` dials a socket path
today; it needs an injected dialer. That is a small change.

Why pipes and not a port: a loopback TCP port is reachable by every local
Windows process and user. Pipes belong to the process that made them.

Child lifetime: the CLI puts `wsl.exe` in a Windows job object so that
closing the CLI ends the child. The bridge exits when its stdin closes. A
daemon that outlives its clients is the existing on-demand daemon, which exits
when no sandboxes exist.

**File sharing.** There is no shared directory between Windows and the guest.
`mgit sandbox sync` and `export` work as on libkrun, with one extra hop:
Windows to distribution, then distribution to guest. The first hop is a tar
stream through `wsl.exe --exec` stdin and stdout. It does not go through
`\\wsl.localhost\`, which is slower and runs through a file-system bridge the
guest-facing design should not depend on.

**Networking.** The guest has no route except through the libkrun gateway
(ADR-010), which runs in the VMM process in the distribution and enforces the
allowlist. The distribution's own network is WSL2's NAT. The gateway dials out
through it. Published ports (SEC-09) go host to guest. On Windows the CLI
reaches them through the same bridge, and `mgit` can listen on `127.0.0.1` on
the Windows side. Whether to offer that in the first version is open.

### Lifecycle on WSL2

| mgit step | WSL2 command | Notes |
|---|---|---|
| `sandbox setup` (new) | `wsl --status`, `wsl --version`, then `wsl --import mgit-sandbox <dir> <rootfs.tar> --version 2` | Needs no administrator rights if the WSL feature is already on. If not, mgit prints `wsl --install --no-distribution` and stops. It does not elevate itself. |
| first launch | `wsl -d mgit-sandbox --user mgit --exec ... mgit-sandboxd ...` | The daemon starts on demand. Starting it starts the WSL VM if it is down, which takes seconds. |
| exec, land, sync, list | through the bridge | Same control protocol as Linux. |
| stop | the daemon stops the microVM | No `wsl` command. The distribution stays up while the daemon runs. |
| idle | none | WSL stops an idle distribution on its own. Running microVMs keep it up. |
| remove | the daemon removes the microVM and its state | Same as Linux. |
| `sandbox teardown` (new) | `wsl --terminate mgit-sandbox`, then `wsl --unregister mgit-sandbox` | Deletes the disk and every staged worktree copy. Asks first. Refuses while a sandbox is `running`, unless forced. |

Two behaviours to design for:

- `wsl --shutdown` or a Windows restart kills every microVM at once. The
  daemon's rehydration marks them `dead`, as it does after a guest crash
  (MGIT-99). Unlanded work in them is gone. Docs must say this plainly.
- The distribution's idle timer can end a daemon with no active client. Active
  microVMs count as running processes, so they keep the distribution up. A
  suspended or created sandbox does not. This needs a test.

`wsl.exe` writes UTF-16 on some of its output. The driver decodes that, runs
`wsl.exe` with a fixed argument list and no shell, and never builds a command
line from task data.

### Virtualization

What needs it:

1. WSL2 itself needs the Virtual Machine Platform feature and hardware
   virtualization enabled in firmware.
2. This design also needs nested virtualization, so that the distribution has
   `/dev/kvm`. WSL2 offers this through `nestedVirtualization` in `.wslconfig`,
   and, from what the documentation says, only on Windows 11. Which CPUs and
   builds work is the first thing the spike confirms.

How mgit detects each, in order. Each is a `doctor` row. A row that could not
run reads `?`, never a pass.

| Check | How | Failure reads |
|---|---|---|
| Windows build | `RtlGetVersion` | "this Windows build cannot run the sandbox" |
| WSL present and current | `wsl --version` exit code and output. The inbox WSL has no `--version`. | "WSL is missing or too old" and the install command |
| Virtual Machine Platform on | `wsl --status`, or starting the distribution and reading the error | "turn on Virtual Machine Platform" |
| Firmware virtualization on | A distribution start that fails with HRESULT `0x80370102`, or `IsProcessorFeaturePresent` with `PF_VIRT_FIRMWARE_ENABLED` | "turn on virtualization in firmware" |
| Nested virtualization | In the distribution: open `/dev/kvm` read-write, as `KVMDeviceCheck` does on Linux | "this machine cannot run a VM inside WSL2" and the `.wslconfig` line |
| libkrun loads | The existing `DaemonLoadsCheck` through the bridge | Existing rows |

The processor feature flags are a hint, not a test. When Windows itself runs
under a hypervisor, they can read as unset on a machine that works. The probe
that decides is the last two rows, which ask the thing that matters.

When any row fails, the launch fails closed with that row's text. It does not
fall back to a weaker mode.

### Security boundaries

The threat model is ADR-005's. The hostile party is the code that runs in the
microVM. What each layer can reach:

| From | Can reach | Cannot reach |
|---|---|---|
| Guest process | The staged worktree copy at the mapped path; its private store; the gateway address | The distribution's disk, the daemon, other guests, Windows |
| After a VMM escape: the VMM process in the distribution | What the unprivileged `mgit` user can read in the distribution: staged copies of other tasks' worktrees and the daemon's state | Windows drives, Windows programs (both switched off), root |
| After that, a kernel escape in the distribution | The whole WSL2 utility VM, including the user's other distributions | Windows, short of a Hyper-V escape |

Points that decide whether this is acceptable:

- **Only the nested microVM isolates.** A hostile guest has to escape a KVM
  guest, then the distribution's user, then the WSL VM. The first step is the
  same as on Linux.
- **The distribution is the trust anchor, not the guest.** The attestation key
  and the registry live in the distribution. A guest escape reaches the
  distribution as an unprivileged user. Key files are readable only by the
  daemon's user, and the VMM runs under a different user. The Linux daemon
  must be checked for the assumption that it and the VMM may share one user.
- **No Windows reach.** `automount` and `interop` are off. The distribution
  keeps the WSL file-server connection that lets Windows read the distribution.
  That path runs outward from Windows, not inward.
- **Windows host address.** In NAT mode the Windows host is the distribution's
  default gateway, in `172.16.0.0/12`. The gateway's allowlist already denies
  private ranges. In `mirrored` mode, the host answers on `127.0.0.1`. The
  gateway must deny loopback. A test must dial both from a guest and see a
  refusal.
- **Hyper-V firewall.** Recent Windows 11 builds can filter the WSL VM's
  traffic to the host. Whether to turn it on, or to require it, is open.
- **Shared kernel.** The kernel in the WSL VM is Microsoft's or the user's,
  not the pinned libkrunfw. A kernel bug in the distribution matters more here
  than on Linux, because the other distributions share it.
- **Land.** The land path stays hardened as ADR-005 describes. Land adds the
  Windows-name checks above. Where the Windows `mgit` verifies hashes again
  after the daemon has verified them is open (see below).

### Why not a distribution per task

A distribution per task is cheap and needs no nested virtualization. It is not
a sandbox. Every distribution runs in the same VM on the same kernel. The
distributions' disks are block devices in that VM. A process that is root in
one distribution can look for the others. Separate mount namespaces slow a
careless process down. They do not stop a hostile one.

That is the same assurance as the container fallback, which ADR-005 allows only
with `--acknowledge-reduced-isolation` and an audit record. If it is built, it
must be that fallback and must go through the same flag. It must never be what
`auto` selects.

## Testing in CI

Hosted Windows runners are virtual machines without nested virtualization.
They can run WSL2's command-line surface but cannot give a distribution
`/dev/kvm`. A Windows machine on bare metal can. So the tests split in two.

**On the hosted Windows runner**, on every pull request:

- Build, vet and unit tests for the Windows target. They already run as a
  cross-build; add a native run.
- The refusal message, with its test (in this change).
- The `wsl.exe` driver against a fake `wsl.exe`: a test program that replays a
  scripted transcript, including UTF-16 output, each failure HRESULT, and a
  hung child. The driver's argument lists are asserted against the table in
  this ADR.
- The path mapping, from a table of Windows paths that includes every edge case
  above. The cases come from the list in this document, not from the mapping
  code.
- The doctor rows. On this runner the nested-virtualization row must read `?`
  or fail. A test that it never reads as a pass here is the negative control
  for the whole column.

**On a bare-metal Windows 11 machine**, registered as a self-hosted runner with
its own label:

- Import the distribution, check `/dev/kvm`, boot a microVM, exec, sync,
  commit, land, export, tear down.
- The hostile-guest battery that the Linux libkrun column runs, plus the two
  Windows-specific refusals: a loopback dial and a gateway-address dial.
- A teardown check that no `mgit-sandbox` distribution and no disk file remain.

Rules for the self-hosted job:

- It does not run for pull requests from forks. A runner that executes
  arbitrary public code defeats the reason for the sandbox.
- It runs on pushes to the main branch, on a schedule, and on demand. It starts
  from a clean machine state, or unregisters its distribution before and after.
- It is not required for merge at first. `docs/E2E-MATRIX.md` lists it as
  **gated** until it has run clean for a period the maintainers choose. Until
  the runner exists, the matrix row for Windows reads "not run".

A release must not claim a Windows sandbox from the hosted job alone. A claim
that the sandbox works on Windows is made only with a recorded run on bare
metal, for the version released.

## Plan

1. **This change.** The refusal message and this document.
2. **Spike, on a bare-metal Windows 11 machine.** Answer the first four open
   questions below. Record the Windows build and CPU for each result. If
   nested KVM does not work, stop and revise this ADR.
3. **Driver.** An `internal/wsl` package with an injected command runner and the
   fake `wsl.exe`. Doctor rows. No behaviour change for users beyond `doctor`.
4. **Bridge.** `--bridge` in the daemon and the injected dialer in the client.
   Run on Linux in CI by replacing `wsl.exe` with a local process.
5. **Setup and teardown.** `sandbox setup` and `sandbox teardown`. The
   distribution image and its pins.
6. **Worktree delivery, path mapping, land checks.**
7. **Self-hosted column, then the matrix row.**

Each step is its own change with its own tests.

## Open questions

1. Does nested KVM work in a WSL2 distribution on the Windows builds and CPUs
   mgit supports: Windows 11 on Intel, Windows 11 on AMD, Windows 11 on Arm,
   and Windows 10? Does libkrun boot on it, and how does boot time compare with
   Linux?
2. Does the VMM run as an unprivileged user in the distribution with only
   `/dev/kvm` access? Does the Linux daemon assume otherwise?
3. What survives a user running `wsl --shutdown`, and what is the user told?
4. Does the distribution's idle shutdown ever stop the daemon while a created or
   suspended sandbox exists?
5. FR-17.3 says "identical absolute path". Is `/mnt/<drive>/...` an acceptable
   amendment, or should the guest path be something else on Windows?
6. Where does Windows-side hash re-verification sit: the daemon verifies and
   attests in the distribution, and the Windows `mgit` imports. Does the Windows
   `mgit` re-hash, and does the attestation need to cover the import?
7. Release size and licence: the Windows archive would carry the Linux daemon
   and libraries, including libkrunfw (GPL-2.0-only kernel). The source offer
   for the Linux archive must extend to it.
8. Is a managed distribution allowed on machines where an administrator
   restricts WSL or `wsl --import`? What does mgit say there?
9. Should the Hyper-V firewall for WSL be required where available?
10. Published ports: is a Windows loopback listener in the first version?
11. Should this and the WCOW backend of ADR-006 both exist, and if so, how does
    a user choose?
12. Does the self-hosted bare-metal runner exist, who runs it, and what is the
    cadence for its job?
