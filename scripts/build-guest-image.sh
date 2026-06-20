#!/usr/bin/env bash
# Builds the Linux guest rootfs image that runs mgit-guest as PID 1 on
# vsock 1024 (MGIT-11.13.4). The image is a SOUP artifact (FR-17.31): it
# is built here, then digest-pinned + Ed25519-signed into images.lock by
# the host signer (internal/sandboxd/images.Register) before use.
#
# Output: an ext4 rootfs containing
#   /sbin/mgit-guest   - the PID-1 supervisor (static, CGO-free)
#   /bin/busybox + sh  - a shell + coreutils for the guest to exec
#   /proc /dev /tmp     - pseudo-fs mount points (mgit-guest mounts them)
#   <worktree-mount>    - the worktree mount point, baked so the read-only
#                         image can mount the worktree over it (until the
#                         guest writable-root lands, MGIT-11.13.5)
#
# Requires (Linux): go, busybox (static), mke2fs (e2fsprogs).
# Usage: build-guest-image.sh <output.ext4> [worktree-mount-path]
set -euo pipefail

OUT="${1:?usage: build-guest-image.sh <output.ext4> [worktree-mount-path]}"
WT_MOUNT="${2:-/sandbox/worktree}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SIZE_MB="${MGIT_GUEST_IMAGE_MB:-128}"

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
mkdir -p "$root"/{sbin,bin,proc,dev,tmp} "$root$WT_MOUNT"

echo "building mgit-guest (static, CGO-free)…"
CGO_ENABLED=0 GOOS=linux go build -C "$REPO_ROOT" -o "$root/sbin/mgit-guest" ./cmd/mgit-guest/

echo "installing busybox shell…"
bb="$(command -v busybox)"
cp "$bb" "$root/bin/busybox"
for applet in sh cat echo ls env pwd; do ln -sf busybox "$root/bin/$applet"; done

echo "packing ext4 rootfs ($SIZE_MB MiB) → $OUT"
rm -f "$OUT"
truncate -s "${SIZE_MB}m" "$OUT"
mke2fs -F -q -t ext4 -d "$root" "$OUT"

echo "done. cmdline: console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"
echo "worktree mount point baked at: $WT_MOUNT"
