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
- **macOS:** Apple Silicon (arm64), macOS 13+. The daemon uses
  Virtualization.framework via CGO and must be code-signed with the
  `com.apple.security.virtualization` entitlement (the release archive and
  Homebrew bottle are already signed; see the go-install caveat below). Intel
  Macs are not supported for the sandbox — they run core mgit only.
- **Windows and everything else:** no sandbox backend yet (epic MGIT-12); core
  mgit runs without containment.

## Installing the host binaries

### Homebrew (recommended)

```bash
brew install hyper-swe/tap/mgit
```

Installs `mgit` and, on Linux and macOS arm64, `mgit-sandboxd` alongside it.
The macOS bottle carries the virtualization entitlement.

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
an *unsigned* binary that lacks the virtualization entitlement, so
Virtualization.framework will refuse to start a VM. Either sign it yourself —

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
integrity. Published, checksummed image bundles ship with the release
(tracked by MGIT-61.2); a signed-by-the-project distribution key is a planned
upgrade (MGIT-61.4).

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
