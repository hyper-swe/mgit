# Reproducible sandbox guest-image build (MGIT-30)

Scripted, SOUP-pinned build of the microVM guest images the sandbox boots — a
kernel + an ext4 rootfs (`mgit-guest` PID 1 + busybox), packaged as an install
**bundle** (`manifest.json` + artifacts) that `mgit sandbox image install
--from <dir>` consumes. This replaces the ad-hoc `/tmp` builds used during the
vzf live validation.

## What "reproducible" means here (SOUP guarantee, FR-17.31)

Every external input is **pinned by a content digest** and every
non-deterministic build knob is **fixed** ([`pins.env`](pins.env)):

- **kernel source** — pinned by `sha256` (a mirror rotation or tamper fails
  loud, never silently builds a different kernel);
- **toolchain + busybox** — pinned by image `@sha256` digest;
- **kernel config** — an explicit symbol list ([`vz-kernel.config`](vz-kernel.config)),
  asserted `=y` in the final `.config`;
- **clock / build identity / fs UUID** — fixed (`SOURCE_DATE_EPOCH`,
  `KBUILD_BUILD_{TIMESTAMP,USER,HOST}`, `ROOTFS_UUID`).

Result:

- **The vz kernel is bit-for-bit reproducible** — two builds from the pinned
  source + toolchain + config produce an identical `Image`; the recorded
  `VZ_KERNEL_IMAGE_SHA256` is asserted on every rebuild (fail-loud on drift).
- **The rootfs *content* is deterministic** — the `mgit-guest` binary is a
  reproducible build (`-trimpath -buildvcs=false -ldflags=-buildid=`), busybox
  is the pinned image's bytes, and every file's mtime is fixed. The ext4
  *container* is **not** bit-reproducible via stock `mke2fs` (it randomizes
  per-inode generation numbers with no flag to fix), so the **built image's
  digest is recorded in the bundle manifest and Ed25519-signed into
  `images.lock`** — the shipped bytes are the verification anchor, checked by
  `mgit sandbox image install` (sha256) and at boot (signature + digest). A
  bit-reproducible rootfs would require a deterministic image tool (e.g. a
  read-only squashfs root) and is tracked as a follow-up.

So the supply-chain property holds end to end: pinned inputs → a recorded,
signed output → verified on install and at boot.

## Prerequisites

- Docker (the kernel + rootfs build in the pinned `linux/arm64` builder image;
  works from an Apple Silicon Mac or a Linux host).
- Go (host toolchain, to build `mgit-guest`).
- `shasum` / `sha256sum`.

## Build a bundle

```bash
# darwin/arm64 (Apple Virtualization.framework): builds the vz kernel + rootfs
scripts/sandbox-image/build-bundle.sh darwin/arm64 out/bundle
```

`build-bundle.sh` writes `out/bundle/{vmlinux-arm64, rootfs-darwin-arm64.ext4,
manifest.json}`. Individual steps:

```bash
scripts/sandbox-image/build-kernel-vz.sh out/vmlinux-arm64   # vz kernel (reproducible)
scripts/sandbox-image/build-rootfs.sh   arm64 out/rootfs.ext4 # rootfs (content-deterministic)
```

`linux/amd64` / `linux/arm64` (firecracker): `build-bundle.sh linux/amd64`
fetches the pinned firecracker kernel (`fetch-kernel-fc.sh`, vendored from
firecracker-ci and sha256-verified) and builds the reproducible rootfs. The
firecracker kernel is vendored (not built by us) — a reproducible-by-us
firecracker kernel is a follow-up; its sha256 pin is the integrity anchor.

## Publish (all platforms)

`publish.sh` builds every platform's bundle into one directory — a combined
`manifest.json` covering all platforms, the per-platform artifacts, and a
`checksums.txt`:

```bash
scripts/sandbox-image/publish.sh out/publish              # darwin/arm64 + linux/{amd64,arm64}
scripts/sandbox-image/publish.sh out/publish linux/amd64  # a subset
```

Attach every file in the output directory to a GitHub release (owner-triggered).
`mgit sandbox image install` with no `--from` then fetches from the latest
release's assets, so a `brew install` user activates the sandbox with one
command.

## Install + run

```bash
cd <your mgit repo>
mgit sandbox image install --from out/bundle    # verifies sha256, signs, registers
mgit work wt --task-id T --sandbox --image <ref>
cd wt && mgit run -- echo ok                     # executes in the guest
```

## macOS: entitlement-sign the daemon (vzf)

Apple's Virtualization.framework refuses to start a VM unless the binary
carries `com.apple.security.virtualization`. Release/brew builds are signed;
a locally-built daemon must be signed once (ad-hoc, no Apple account):

```bash
codesign --force --sign - --entitlements build/darwin/vz.entitlements $(command -v mgit-sandboxd)
```

## Recording the kernel digest

After the first reproducible kernel build, record its `Image` sha256 in
`pins.env` as `VZ_KERNEL_IMAGE_SHA256=`; subsequent builds assert against it.

## Constraints

- No AI-attribution trailers in commits.
- The KVM validation host is referred to as "the Linux runner" — never its
  LAN IP, and never commit the IP.
