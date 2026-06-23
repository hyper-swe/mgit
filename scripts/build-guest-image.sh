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
#   /mnt                - tmpfs scratch for the writable-root overlay
#                         (mgit-guest overlays a writable root over this
#                         read-only image so it can create the worktree's
#                         identical mount point at runtime, MGIT-11.6.6)
#
# The worktree mount point is NOT baked: mgit-guest creates it on the
# writable overlay root, so any host worktree path works.
#
# Requires (Linux): go, busybox (static), mke2fs (e2fsprogs).
# Usage: build-guest-image.sh <output.ext4>
set -euo pipefail

OUT="${1:?usage: build-guest-image.sh <output.ext4>}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SIZE_MB="${MGIT_GUEST_IMAGE_MB:-128}"

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
mkdir -p "$root"/{sbin,bin,proc,dev,tmp,mnt}

echo "building mgit-guest (static, CGO-free)…"
CGO_ENABLED=0 GOOS=linux go build -C "$REPO_ROOT" -o "$root/sbin/mgit-guest" ./cmd/mgit-guest/

echo "installing busybox shell…"
bb="$(command -v busybox)"
cp "$bb" "$root/bin/busybox"
# Shell + coreutils for the guest to exec, plus the network applets the
# FR-17.7 network-enforcement e2e needs to probe egress from inside the
# guest (TCP connect via nc, the proxy CONNECT handshake, DNS via nslookup,
# raw-IP attempts). The applets are already compiled into the static
# busybox; these are just symlinks. Refs: FR-17.7, MGIT-11.13.6
for applet in sh cat echo ls env pwd printf sleep \
              nc wget nslookup ping ip ifconfig route; do
	ln -sf busybox "$root/bin/$applet"
done

echo "packing ext4 rootfs ($SIZE_MB MiB) → $OUT"
rm -f "$OUT"
truncate -s "${SIZE_MB}m" "$OUT"
mke2fs -F -q -t ext4 -d "$root" "$OUT"

# ipv6.disable=1 is REQUIRED for SEC-04: the allowlist tap firewall is
# IPv4-only, so the guest must have no IPv6 stack (no un-firewalled v6 egress
# path). Register the image with this in its cmdline. Refs: SEC-04, FR-17.7
echo "done. cmdline: console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"
