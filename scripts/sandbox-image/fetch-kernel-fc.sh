#!/usr/bin/env bash
# Fetch the pinned firecracker (Linux/KVM) guest kernel for an arch and verify
# its sha256 against pins.env (fail-loud on mismatch). The firecracker kernel
# is VENDORED (firecracker-ci prebuilt) rather than built by us; the sha256 pin
# is the integrity anchor. Refs: MGIT-61.2
#
# Usage: fetch-kernel-fc.sh <amd64|arm64> <out-vmlinux>
set -euo pipefail

ARCH="${1:?usage: fetch-kernel-fc.sh <amd64|arm64> <out-vmlinux>}"
OUT="${2:?usage: fetch-kernel-fc.sh <amd64|arm64> <out-vmlinux>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=pins.env
. "$HERE/pins.env"

case "$ARCH" in
amd64) url="$FC_KERNEL_AMD64_URL"; want="$FC_KERNEL_AMD64_SHA256" ;;
arm64) url="$FC_KERNEL_ARM64_URL"; want="$FC_KERNEL_ARM64_SHA256" ;;
*) echo "FATAL: unknown arch $ARCH (want amd64|arm64)" >&2; exit 2 ;;
esac
if [ -z "$url" ] || [ -z "$want" ]; then
	echo "FATAL: firecracker kernel not pinned for $ARCH (set FC_KERNEL_${ARCH^^}_* in pins.env)" >&2
	exit 2
fi

echo "fetching firecracker kernel ($ARCH): $url"
curl -fsSL "$url" -o "$OUT"
got="$(shasum -a 256 "$OUT" | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
	echo "FATAL: firecracker kernel sha256 mismatch ($ARCH): got $got, pinned $want" >&2
	rm -f "$OUT"
	exit 1
fi
echo "firecracker kernel sha256 OK: $got"
