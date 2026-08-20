#!/usr/bin/env bash
# Fetch the pinned firecracker VMM binary for an arch and verify its sha256
# against pins.env (fail-loud on mismatch), then install it at <out>.
#
# The VMM is a SOUP input exactly like the guest kernel: mgit does not build
# firecracker, it execs it, so the sha256 pin is the integrity anchor. Mirrors
# fetch-kernel-fc.sh deliberately — one shape for every vendored artifact.
#
# This exists because nothing in the repo installed the VMM: the Linux live
# pass assumed a hand-provisioned KVM box that already had `firecracker` on
# PATH, which is precisely what made that gate un-runnable anywhere else.
# Refs: MGIT-78, FR-17.15, ADR-005
#
# Usage: fetch-firecracker.sh <amd64|arm64> <out-binary>
set -euo pipefail

ARCH="${1:?usage: fetch-firecracker.sh <amd64|arm64> <out-binary>}"
OUT="${2:?usage: fetch-firecracker.sh <amd64|arm64> <out-binary>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=pins.env
. "$HERE/pins.env"

case "$ARCH" in
amd64) url="$FC_VMM_AMD64_URL"; want="$FC_VMM_AMD64_SHA256"; inner="x86_64" ;;
arm64) url="$FC_VMM_ARM64_URL"; want="$FC_VMM_ARM64_SHA256"; inner="aarch64" ;;
*) echo "FATAL: unknown arch $ARCH (want amd64|arm64)" >&2; exit 2 ;;
esac
if [ -z "$url" ] || [ -z "$want" ]; then
	echo "FATAL: firecracker VMM not pinned for $ARCH (set FC_VMM_${ARCH}_* in pins.env)" >&2
	exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "fetching firecracker VMM ($ARCH): $url"
# -t 120 is the MGIT-143 floor: this step's worst SUCCESSFUL run is 3s across
# n=131, and 4x that is far below the floor. The restore removes a truncated
# download between attempts. The EXIT trap above does not cover this: it fires
# when the script exits, which is exactly not when a retry needs the partial
# gone. Refs: MGIT-143 clause 3
"$HERE/../ci/guard-fetch.sh" -t 120 -l firecracker-vmm \
	-c "rm -f '$tmp/fc.tgz'" -- \
	curl -fsSL "$url" -o "$tmp/fc.tgz"
got="$(shasum -a 256 "$tmp/fc.tgz" | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
	echo "FATAL: firecracker VMM sha256 mismatch ($ARCH): got $got, pinned $want" >&2
	exit 1
fi
echo "firecracker VMM sha256 OK: $got"

tar -C "$tmp" -xzf "$tmp/fc.tgz"
src="$tmp/release-${FC_VMM_VERSION}-${inner}/firecracker-${FC_VMM_VERSION}-${inner}"
[ -x "$src" ] || { echo "FATAL: $src missing from the release archive" >&2; exit 1; }
install -m 0755 "$src" "$OUT"
"$OUT" --version | head -1
