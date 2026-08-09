# Installing the mgit sandbox (mgit-sandboxd + the guest base)

This is the distribution reference for mgit's containment pillar. Core mgit —
commit, worktree, squash, land-by-patch — works from a single `mgit` binary
with nothing here. You only need this page to turn on the **per-task microVM
sandbox** (`mgit run`, `mgit work --sandbox`).

> The README's "Enable the sandbox" walkthrough links here for the mechanics.

## The pieces

The sandbox has three distribution artifacts:

| Artifact | What it is | Where it lives |
|----------|-----------|----------------|
| `mgit` | Core CLI (pure Go, no CGO). | Host `PATH`. |
| `mgit-sandboxd` | Per-platform host daemon that owns the VMM (FR-17.16). | Host, **next to `mgit`** or on `PATH`. |
| Guest base | The Linux userspace the microVM boots; runs `mgit-guest` as PID 1. Under libkrun it is a **directory** you compose from any OCI image; under firecracker/vzf a kernel + ext4 rootfs. | Per repo, digest-pinned in `.mgit/sandbox/images.lock`. **Not** on host `PATH`. |

`mgit` locates `mgit-sandboxd` beside its own executable first, then on `PATH`
(`cmd/mgit/sandbox_connect.go`). Installing both into the same directory — which
every channel below does — is what makes `mgit run` find the daemon.

## Platform prerequisites

- **Linux:** KVM (`/dev/kvm` present and accessible) and the `firecracker`
  binary on `PATH`. The daemon is pure Go and needs no CGO.
- **macOS:** Apple Silicon (arm64), **macOS 14+**. The daemon links **libkrun**
  — the default backend since GA (ADR-010) — via CGO, and must be code-signed
  with the `com.apple.security.hypervisor` entitlement (the release archive and
  Homebrew bottle are already signed; see the go-install caveat below).
  **`brew install hyper-swe/tap/mgit` does not install libkrun**; you install
  it yourself, once, as the [step below](#installing-libkrun-on-macos). Intel
  Macs are not supported for the sandbox — they run core mgit only.

  The older Virtualization.framework backend (vzf, macOS 13+) remains in the
  tree behind `-tags vzf` and is not shipped. It is not a supported
  configuration; it exists so the seam stays exercised.
- **Windows and everything else:** no sandbox backend yet (epic MGIT-12); core
  mgit runs without containment.

### Installing libkrun on macOS

libkrun lives in a third-party Homebrew tap, and Homebrew will not load a
formula from a tap you have not trusted. Trust it first, then install:

```bash
brew tap libkrun/krun
brew trust libkrun/krun
brew install libkrun
```

**All three commands are needed, in that order.** `brew install libkrun` on
its own fails with *"Refusing to load formula libkrun/krun/libkrun from
untrusted tap"*, and so does the fully-qualified `brew install
libkrun/krun/libkrun` — one step later, on `libkrunfw`, which libkrun itself
depends on from the same tap and which no command-line argument can whitelist.
Whole-tap `brew trust` is what clears both.

> **Why mgit does not install it for you.** It used to try: the formula
> declared libkrun as a dependency, and because Homebrew resolves dependencies
> before it fetches anything, `brew install hyper-swe/tap/mgit` aborted on the
> untrusted tap and installed *nothing at all* — not even core mgit, which
> never links libkrun. Core mgit is CGO-free and needs no hypervisor, so the
> trust decision about a third-party VMM now belongs to the people who
> actually want a sandbox. Refs: MGIT-75

If you skip this step and try to start a sandbox anyway, nothing silently
degrades: the daemon cannot load, and `mgit` reports the dynamic loader's
error together with the three commands above.

### libkrun builds must have networking enabled

Builds that link the **libkrun** backend — every macOS build, and Linux builds
using `-tags libkrun` — need a libkrun **built with networking support**. This is not the default: upstream gates the
`krun_add_net_*` API behind an opt-in build flag, and a libkrun built without
it exports none of those symbols while still declaring them in its header — so
the failure is a bare missing-symbol error at link time.

It is a hard prerequisite rather than a nice-to-have: mgit attaches an explicit
network device to **every** sandbox in **every** mode, including `none`. With
no net device libkrun falls back to TSI (Transparent Socket Impersonation),
which proxies the guest's sockets through the host and hands it full egress. So
there is no NIC-less mode to degrade to — a libkrun without networking cannot
host a sandbox at all, and mgit-sandboxd refuses to start against one.

Check the library you have:

```bash
# macOS
nm -gU "$(brew --prefix libkrun)/lib/libkrun.dylib" | grep krun_add_net_unixgram
# Linux
nm -D /usr/lib/libkrun.so | grep krun_add_net_unixgram
```

A match means networking is enabled. If there is none, rebuild libkrun with
`make NET=1` (upstream `containers/libkrun`), or install a package that enables
it. The Homebrew `libkrun/krun` tap passes `NET=1` explicitly, so the brew path
is covered.

### Building mgit-sandboxd from source on macOS

Because libkrun is linked rather than tag-gated, cgo must find its
pkg-config. `make` derives this automatically from Homebrew; a raw `go build`
needs it exported:

```bash
export PKG_CONFIG_PATH="$(brew --prefix libkrun)/lib/pkgconfig"
go build ./cmd/mgit-sandboxd/
```

Core `mgit` is unaffected — it is CGO-free and never links libkrun:

```bash
CGO_ENABLED=0 go build ./cmd/mgit/
```

## Installing the host binaries

### Homebrew (recommended)

```bash
brew install hyper-swe/tap/mgit
```

Needs no other tap and no `brew trust`. Installs `mgit` and, on Linux and
macOS arm64, `mgit-sandboxd` alongside it. The macOS bottle is signed with
both the hypervisor (libkrun) and virtualization (vzf) entitlements.

On macOS this gets you core mgit and the daemon binary, but **not** the
hypervisor the daemon links — install libkrun separately
([above](#installing-libkrun-on-macos)) when you want the sandbox.

Whether a brew install is affected by the Gatekeeper quarantine issue below
is not yet verified — see the note in "Release archive".

### Release archive

Download `mgit_<version>_<os>_<arch>.tar.gz` from the
[releases](https://github.com/hyper-swe/mgit/releases) page. Linux and
macOS-arm64 archives contain **both** binaries; extract them into one directory
on your `PATH`. (Windows and Intel-macOS archives contain `mgit` only.)

**macOS: a downloaded archive will not run until you clear quarantine.**
Any transfer that sets the `com.apple.quarantine` extended attribute — a
browser download, AirDrop, anything other than `scp` or a local build —
triggers this. The binaries are signed ad-hoc (`codesign --sign -`, no
notarization yet), and on Apple Silicon Gatekeeper kills a quarantined
ad-hoc-signed binary outright: `spctl -a -vv ./mgit-sandboxd` reports
`rejected`, and running either binary gets you `zsh: killed` with no dialog
and no explanation. **Both `mgit` and `mgit-sandboxd` are affected.** The
remedy is complete and verified — confirmed on a second Mac:

```bash
xattr -d com.apple.quarantine mgit mgit-sandboxd
```

After that, both binaries run normally; the binaries themselves are fine,
this is purely a distribution/signing gap.

> Whether a Homebrew install carries the same problem is **not yet
> verified** — brew's own install step may or may not clear the quarantine
> attribute it inherits from whatever fetched the bottle. Treat this as an
> open question until confirmed one way or the other; update this note with
> the answer rather than assuming either direction. Refs: MGIT-64

### go install

```bash
# Core mgit — every platform
go install github.com/hyper-swe/mgit/cmd/mgit@latest

# The sandbox daemon
go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest
```

`go install` of the daemon works fully **on Linux**. **On macOS** it produces
an *unsigned* binary that lacks the hypervisor entitlement, so libkrun will
refuse to start a VM (and it needs `PKG_CONFIG_PATH` set at build time, above).
Either sign it yourself —

```bash
codesign --force --sign - \
  --entitlements "$(go env GOPATH)/pkg/mod/github.com/hyper-swe/mgit@*/build/darwin/vz.entitlements" \
  "$(go env GOPATH)/bin/mgit-sandboxd"
```

— or, more simply, use Homebrew or the release archive on macOS.

## Provisioning the guest base

What the daemon boots depends on the backend, and the two are genuinely
different shapes:

| Backend | What it boots | How you provision it |
|---------|---------------|----------------------|
| **libkrun** (macOS default, and Linux with `-tags libkrun`) | A **directory**: libkrunfw supplies the kernel, and the guest root is shared over virtio-fs. | `mgit sandbox base from <oci-image>` |
| firecracker (Linux) / vzf (`-tags vzf`) | A kernel + ext4 **rootfs image**. | `mgit sandbox image install` |

If you are on macOS, you want the first row.

### Compose a base from any Linux image (libkrun)

```bash
mgit sandbox image init                 # once per repo: create the signing trust root
mgit sandbox base from debian:12        # pull, compose, inject, pin, sign
```

That pulls a public OCI image straight from its registry — no Docker, no
container runtime, no daemon — extracts it into `.mgit/sandbox/base`, injects
`mgit` and `mgit-guest`, then pins the composed tree by content digest and
signs it into `images.lock`. `mgit run` and `mgit work --sandbox` use it
automatically from then on; you never retype the digest.

**Pick the image your task's toolchain needs.** The base IS the environment
your agent works in, so start from something that already carries it:

```bash
mgit sandbox base from node:22          # JS/TS
mgit sandbox base from python:3.12      # Python
mgit sandbox base from golang:1.23      # Go
mgit sandbox base from debian:12        # general-purpose
```

Anything that is a Linux userspace works. Scratch and distroless images do
not — they have no shell, so an agent could not run a command in one, and
`base from` refuses them by name rather than letting you find out later.

**mgit ships no default base, deliberately.** With none registered, launching
fails closed and names the command above. That is not an oversight: mgit
redistributes no kernel and no userspace, and silently booting some image we
picked for you would put code you never chose inside your containment
boundary.

**Why this is safe even though you choose the contents.** The guest is the
UNTRUSTED side — that is the entire point of the VM boundary, and a poisoned
base burns a throwaway microVM. Everything that must stay protected is
enforced host-side and is unaffected by what the base contains: the private
store quarantine (SEC-03), the egress policy, the land airlock, and
attestation signing. What mgit still owes you is integrity of what you asked
for, so every blob is verified against its digest as it is pulled, and the
composed tree is pinned and signed before anything boots it.

Two things `base from` will warn or refuse about, because both fail
confusingly later:

- **Architecture.** libkrun uses hardware virtualization; there is no
  emulation to cross architectures with. An image built for another
  architecture is refused, naming both.
- **C library.** A glibc-linked tool inside a musl (alpine) base dies with
  "no such file or directory" naming its dynamic *loader*, not the binary you
  ran. A musl base is a legitimate choice, so this is a warning, not a
  refusal.

### Where the guest binaries come from

`mgit-guest` is PID 1 inside the guest and is meaningless on a host. Each
release archive therefore ships linux builds of `mgit` and `mgit-guest` in a
`guest/` directory beside the host binary, and `base from` injects them from
there — which is what makes a plain `brew install` enough to compose a base
with no Go toolchain and no source checkout.

They are always injected by mgit, overwriting whatever the image had at those
paths. An image that ships its own `/sbin/mgit-guest` must never end up
mediating exec, land and the control plane.

From a source checkout, `base from` cross-builds them instead. To use builds
you made yourself, pass `--guest-bin-dir <dir>` containing `mgit` and
`mgit-guest` built for `linux/<arch>`.

### Bring your own directory

If you already have a Linux userspace tree — debootstrap output, an unpacked
container export, anything — register it directly:

```bash
mgit sandbox base set /path/to/rootfs-tree
```

Unlike `base from`, this does not write into the tree beyond injecting the two
mgit binaries: it is yours, so a tree missing the mount points the supervisor
needs (`/proc`, `/dev`, `/tmp`, `/mnt`) is reported rather than silently
completed.

### Install a kernel + rootfs image (firecracker / vzf)

The older backends boot a digest-pinned kernel and ext4 rootfs instead:

```bash
mgit sandbox image install                     # from the shipped release bundle
mgit sandbox image install --from <dir-or-url> # or a local dir / your own build
```

A `--from` source is a directory or `https://` base holding a `manifest.json`
plus the named `kernel` and `rootfs` artifacts. `manifest.json` maps
`"os/arch"` to the platform's artifacts, their pinned `sha256`, and the guest
`cmdline`:

```json
{
  "schema": 1,
  "images": {
    "linux/amd64":  { "kernel": "vmlinux", "kernel_sha256": "sha256:…", "rootfs": "rootfs-linux-amd64.ext4",  "rootfs_sha256": "sha256:…", "cmdline": "console=ttyS0 … root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest" },
    "darwin/arm64": { "kernel": "vmlinux-arm64", "kernel_sha256": "sha256:…", "rootfs": "rootfs-darwin-arm64.ext4", "rootfs_sha256": "sha256:…", "cmdline": "console=hvc0 root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest" }
  }
}
```

Install fails closed on any digest mismatch and is idempotent. Build your own
with `scripts/build-guest-image.sh out/rootfs.ext4`, or register one directly
with `mgit sandbox image add --kernel … --rootfs … --cmdline …`. The
reproducible, SOUP-pinned kernel + rootfs build is tracked by **MGIT-30**.

**Publishing is on hold (MGIT-61.12, ⛔ see
[RELEASE-CHECKLIST.md](release/RELEASE-CHECKLIST.md)):** the owner deferred
attaching bundles to releases, since publishing today would hand out an
artifact the libkrun migration intends to retire — and `base from` removes the
need for it. **`mgit sandbox image install` with no `--from` will not find
anything to fetch** — use `--from <local bundle dir>` (built with
`scripts/sandbox-image/build-bundle.sh`). The mechanism itself is unchanged
and live-validated; only the "attach to a GitHub release" step is paused.

## Trust model

Whichever shape you provision, it is pinned by content digest and Ed25519
signed into **your repo's own** trust root (local-trust), and verified again
at boot: a base that changed under a running task fails the launch rather than
booting quietly. For `base from`, the resolved OCI reference — registry,
repository, tag and the digest the registry actually served — is recorded
alongside the tree digest and covered by the same signature, so provenance
cannot be rewritten without detection. A signed-by-the-project distribution
key is a separate, later upgrade (MGIT-61.4).

## Distribution decision: why the guest binary is not on host PATH

`mgit-guest` refuses to run off Linux and is PID 1 inside the microVM. On the
host `PATH` it would be misleading — an agent could invoke it and get nothing
useful. So the boundary is:

- **Host channels (brew / archive / go install)** put `mgit` + `mgit-sandboxd`
  on `PATH`.
- **The archive additionally carries** linux `mgit` + `mgit-guest` under
  `guest/`, not on `PATH`, for injection into a base.
- **A kernel+rootfs image** (firecracker / vzf) carries `mgit-guest` inside it,
  pinned in `images.lock`.

Refs: MGIT-44, MGIT-30, MGIT-61.15, ADR-005, ADR-010, FR-17.15, FR-17.16
