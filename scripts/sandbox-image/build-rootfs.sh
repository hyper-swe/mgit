#!/usr/bin/env bash
# Reproducibly build the guest rootfs ext4 for a target arch: a static,
# CGO-free mgit-guest as PID 1 plus the pinned busybox shell/toolchain. The
# tree assembly + mke2fs run INSIDE the pinned builder image (GNU coreutils +
# e2fsprogs) with a fixed UUID and SOURCE_DATE_EPOCH, so the output is
# byte-reproducible given the pins. Generalizes scripts/build-guest-image.sh
# (which is host-tool + host-arch bound). Refs: MGIT-30, FR-17.31, SEC-03
#
# Usage: build-rootfs.sh <goarch: amd64|arm64> <out.ext4>
#   Env: MGIT_GUEST_IMAGE_MB (default 128)
set -euo pipefail

ARCH="${1:?usage: build-rootfs.sh <amd64|arm64> <out.ext4>}"
OUT="${2:?usage: build-rootfs.sh <amd64|arm64> <out.ext4>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
# shellcheck source=pins.env
. "$HERE/pins.env"
SIZE_MB="${MGIT_GUEST_IMAGE_MB:-128}"
OUT="$(cd "$(dirname "$OUT")" && pwd)/$(basename "$OUT")"

stage="$(mktemp -d)"; trap 'rm -rf "$stage"' EXIT

# 1) mgit-guest — reproducible static build (trimpath + no VCS + no buildid).
echo "building mgit-guest ($ARCH, static, reproducible)…"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -C "$REPO_ROOT" \
	-trimpath -buildvcs=false -ldflags='-buildid=' \
	-o "$stage/mgit-guest" ./cmd/mgit-guest/

# 2) busybox — extract the pinned per-arch image's static binary. The digest
# is arch-specific, so no --platform (which is unreliable with an index digest).
case "$ARCH" in
amd64) busybox_image="$BUSYBOX_AMD64"; builder_image="$BUILDER_AMD64" ;;
arm64) busybox_image="$BUSYBOX_ARM64"; builder_image="$BUILDER_ARM64" ;;
*) echo "FATAL: no pins for arch $ARCH" >&2; exit 2 ;;
esac
echo "extracting busybox from ${busybox_image}"
cid="$(docker create "$busybox_image")"
docker cp "$cid:/bin/busybox" "$stage/busybox" >/dev/null
docker rm "$cid" >/dev/null

# 3) Assemble the tree + pack ext4 in the pinned builder (deterministic). The
# arch-specific builder digest selects the arch (docker emulates if cross), so
# no --platform (which is unreliable with a multi-arch index digest).
docker run --rm \
	-e "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
	-e "ROOTFS_UUID=$ROOTFS_UUID" \
	-e "SIZE_MB=$SIZE_MB" \
	-v "$stage:/stage:ro" \
	-v "$(dirname "$OUT"):/out" \
	"$builder_image" bash -euo pipefail -c '
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq
		apt-get install -y -qq e2fsprogs >/dev/null
		root="$(mktemp -d)"
		mkdir -p "$root"/{sbin,bin,proc,dev,tmp,mnt}
		install -m 0755 /stage/mgit-guest "$root/sbin/mgit-guest"
		install -m 0755 /stage/busybox    "$root/bin/busybox"
		# Same applet set as build-guest-image.sh (shell/coreutils + network
		# probes for SEC-04 e2e + fs probes for SEC-03 + mke2fs for the COW
		# overlay). All are the one static busybox.
		for a in sh cat echo ls env pwd printf sleep test touch mkdir dd sync \
		         grep find head awk mke2fs nc wget nslookup ping ip ifconfig route; do
			ln -sf busybox "$root/bin/$a"
		done
		# Fix every entry mtime to the pinned clock so the image is reproducible.
		find "$root" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
		out="/out/'"$(basename "$OUT")"'"
		rm -f "$out"; truncate -s "${SIZE_MB}m" "$out"
		# Fixed UUID + hash_seed + SOURCE_DATE_EPOCH → byte-reproducible ext4.
		mke2fs -F -q -t ext4 -U "$ROOTFS_UUID" -E "hash_seed=$ROOTFS_UUID" -d "$root" "$out"
	'

sha="$(shasum -a 256 "$OUT" | cut -d' ' -f1)"
echo "built rootfs: $OUT"
echo "  sha256: $sha"
