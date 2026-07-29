# Installing the mgit sandbox (mgit-sandboxd + guest image)

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
| Guest image (kernel + rootfs) | The Linux microVM the daemon boots; runs `mgit-guest` as PID 1. | Inside the image, digest-pinned in `images.lock`. **Not** on host `PATH`. |

`mgit` locates `mgit-sandboxd` beside its own executable first, then on `PATH`
(`cmd/mgit/sandbox_connect.go`). Installing both into the same directory — which
every channel below does — is what makes `mgit run` find the daemon.

## Platform prerequisites

- **Linux:** KVM (`/dev/kvm` present and accessible) and the `firecracker`
  binary on `PATH`. The daemon is pure Go and needs no CGO.
- **macOS:** Apple Silicon (arm64), **macOS 14+**. The daemon links **libkrun**
  — the default backend since GA (ADR-010) — via CGO, and must be code-signed
  with the `com.apple.security.hypervisor` entitlement (the release archive and
  Homebrew bottle are already signed; see the go-install caveat below). libkrun
  is a hard dependency: `brew install hyper-swe/tap/mgit` pulls it in. Intel
  Macs are not supported for the sandbox — they run core mgit only.

  The older Virtualization.framework backend (vzf, macOS 13+) remains in the
  tree behind `-tags vzf` and is not shipped. It is not a supported
  configuration; it exists so the seam stays exercised.
- **Windows and everything else:** no sandbox backend yet (epic MGIT-12); core
  mgit runs without containment.

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

Installs `mgit` and, on Linux and macOS arm64, `mgit-sandboxd` alongside it,
pulling in libkrun on macOS. The macOS bottle is signed with both the
hypervisor (libkrun) and virtualization (vzf) entitlements.

### Release archive

Download `mgit_<version>_<os>_<arch>.tar.gz` from the
[releases](https://github.com/hyper-swe/mgit/releases) page. Linux and
macOS-arm64 archives contain **both** binaries; extract them into one directory
on your `PATH`. (Windows and Intel-macOS archives contain `mgit` only.)

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

## Provisioning the guest image

The daemon boots a Linux microVM from a digest-pinned kernel + rootfs. The
rootfs bakes in `mgit-guest` (the PID-1 supervisor) plus a busybox shell and
toolchain; **`mgit-guest` is never a host binary** — it only has meaning inside
the guest, so it is not shipped on `PATH` and not in the release archives.

### Install a shipped image (recommended)

From within an mgit repo, one command fetches a pinned image **bundle** for
your platform, verifies each artifact's sha256, sets up the local signing
trust root if needed, and registers the digest-pinned, signed image:

```bash
mgit sandbox image install                     # from the shipped release bundle
mgit sandbox image install --from <dir-or-url> # or a local dir / your own build
```

With no `--from`, install fetches from the latest mgit release's published
bundle (the release attaches per-platform artifacts + `manifest.json`). A
`--from` source is a directory or `https://` base holding a `manifest.json`
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

Install fails closed on any digest mismatch and is idempotent. `mgit run` and
`mgit work --sandbox` then use the registered image automatically. **Trust
model:** the image is digest-pinned and Ed25519-signed into your repo's own
trust root (local-trust); the `sha256` pin plus HTTPS provide distribution
integrity.

**Publishing is currently on hold (MGIT-61.12, ⛔ see
[RELEASE-CHECKLIST.md](release/RELEASE-CHECKLIST.md)):** the owner deferred
attaching bundles to releases until the libkrun consolidation lands, since
publishing today would hand out an artifact this migration intends to
retire. **`mgit sandbox image install` with no `--from` will not find
anything to fetch yet** — use `--from <local bundle dir>` (built with
`scripts/sandbox-image/build-bundle.sh`, below) until the hold lifts. The
mechanism (digest-pinned bundle, sha256 verification, `manifest.json` schema)
is unchanged and already live-validated end to end; only the "attach to a
GitHub release" step is paused. A signed-by-the-project distribution key is a
separate, later upgrade (MGIT-61.4).

### Build your own image

```bash
scripts/build-guest-image.sh out/rootfs.ext4
```

then either point `mgit sandbox image install --from <dir>` at a directory
containing a hand-written `manifest.json` + your kernel/rootfs, or register
directly with `mgit sandbox image init` + `mgit sandbox image add --kernel …
--rootfs … --cmdline …`. The reproducible, SOUP-pinned kernel + rootfs build
(both backends) is tracked by **MGIT-30**.

## Distribution decision: why the guest binary is not shipped on the host

`mgit-guest` is `//go:build linux`-only in effect (it refuses to run off
Linux) and is PID 1 inside the microVM. Shipping it on the host `PATH` would be
misleading — an agent could invoke it and get nothing useful. So the
distribution boundary is:

- **Host channels (brew / archive / go install)** ship `mgit` + `mgit-sandboxd`.
- **The guest image** carries `mgit-guest`, built from this repo by
  `scripts/build-guest-image.sh` and pinned in `images.lock`.

Refs: MGIT-44, MGIT-30, ADR-005, FR-17.15, FR-17.16
