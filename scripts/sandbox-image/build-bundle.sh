#!/usr/bin/env bash
# Build a complete, pinned guest-image BUNDLE for a platform: the kernel + the
# rootfs + a manifest.json entry (with recorded sha256 digests) that
# `mgit sandbox image install --from <bundle-dir>` consumes. Refs: MGIT-30,
# MGIT-61.1, MGIT-61.2
#
# Usage: build-bundle.sh <platform: darwin/arm64|linux/amd64|linux/arm64> <bundle-dir>
set -euo pipefail

PLATFORM="${1:?usage: build-bundle.sh <platform> <bundle-dir>}"
DIR="${2:?usage: build-bundle.sh <platform> <bundle-dir>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$DIR"; DIR="$(cd "$DIR" && pwd)"

FC_CMDLINE="console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 random.trust_cpu=on root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"
VZ_CMDLINE="console=hvc0 root=/dev/vda ro rootfstype=ext4 init=/sbin/mgit-guest"

case "$PLATFORM" in
darwin/arm64)
	kernel="vmlinux-arm64"; rootfs="rootfs-darwin-arm64.ext4"; cmdline="$VZ_CMDLINE"
	echo "== $PLATFORM: build vz kernel =="
	bash "$HERE/build-kernel-vz.sh" "$DIR/$kernel"
	echo "== $PLATFORM: build arm64 rootfs =="
	bash "$HERE/build-rootfs.sh" arm64 "$DIR/$rootfs"
	;;
linux/amd64|linux/arm64)
	echo "FATAL: $PLATFORM firecracker kernel vendoring is not wired yet (MGIT-61.2)." >&2
	echo "  The rootfs build works: bash $HERE/build-rootfs.sh ${PLATFORM#linux/} <out.ext4>" >&2
	echo "  Pin FC_KERNEL_* in pins.env, then extend this case to vendor+verify the vmlinux." >&2
	exit 2
	;;
*)
	echo "FATAL: unknown platform $PLATFORM (want darwin/arm64|linux/amd64|linux/arm64)" >&2
	exit 2
	;;
esac

ksha="sha256:$(shasum -a 256 "$DIR/$kernel" | cut -d' ' -f1)"
rsha="sha256:$(shasum -a 256 "$DIR/$rootfs" | cut -d' ' -f1)"

# Merge this platform's entry into manifest.json (create or update), sorted +
# stable so the file is reproducible.
python3 - "$DIR/manifest.json" "$PLATFORM" "$kernel" "$ksha" "$rootfs" "$rsha" "$cmdline" <<'PY'
import json, os, sys
path, plat, kernel, ksha, rootfs, rsha, cmdline = sys.argv[1:8]
m = {"schema": 1, "images": {}}
if os.path.exists(path):
    with open(path) as f:
        m = json.load(f)
m.setdefault("schema", 1)
m.setdefault("images", {})
m["images"][plat] = {
    "kernel": kernel, "kernel_sha256": ksha,
    "rootfs": rootfs, "rootfs_sha256": rsha,
    "cmdline": cmdline,
}
m["images"] = dict(sorted(m["images"].items()))
with open(path, "w") as f:
    json.dump(m, f, indent=2, sort_keys=True)
    f.write("\n")
PY

echo "== bundle updated: $DIR/manifest.json =="
cat "$DIR/manifest.json"
