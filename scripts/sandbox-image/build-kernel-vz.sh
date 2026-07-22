#!/usr/bin/env bash
# Reproducibly build the arm64 vz (Apple Virtualization.framework) guest
# kernel from pinned source + config, in the pinned builder image, with the
# determinism knobs from pins.env. Prints the output Image's sha256 and, when
# VZ_KERNEL_IMAGE_SHA256 is pinned, ASSERTS the build matches it (fail-loud on
# drift). Refs: MGIT-30, FR-17.31
#
# Usage: build-kernel-vz.sh <out-Image>
#   Env: VZ_KERNEL_SRC=<local tarball>  use a vendored source instead of the URL
set -euo pipefail

OUT="${1:?usage: build-kernel-vz.sh <out-Image>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=pins.env
. "$HERE/pins.env"

OUT="$(cd "$(dirname "$OUT")" && pwd)/$(basename "$OUT")"
config_symbols="$(grep -vE '^\s*#|^\s*$' "$HERE/vz-kernel.config" | tr '\n' ' ')"

# Fetch (or use a vendored) source tarball and verify its sha256 before build.
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
tarball="$work/linux.tar.xz"
if [ -n "${VZ_KERNEL_SRC:-}" ]; then
	cp "$VZ_KERNEL_SRC" "$tarball"
else
	echo "fetching $VZ_KERNEL_URL"
	curl -fsSL "$VZ_KERNEL_URL" -o "$tarball"
fi
got="$(shasum -a 256 "$tarball" | cut -d' ' -f1)"
if [ "$got" != "$VZ_KERNEL_SHA256" ]; then
	echo "FATAL: kernel source sha256 mismatch: got $got, pinned $VZ_KERNEL_SHA256" >&2
	echo "  (mirror rotated or tampered — vendor the pinned tarball and set VZ_KERNEL_SRC)" >&2
	exit 1
fi
echo "kernel source sha256 OK: $got"

# Build in the pinned builder image (arm64), with fixed timestamp/user/host so
# the Image is reproducible given the same pinned toolchain digest.
# The vz kernel is arm64-only; use the arch-pinned arm64 builder digest (no
# --platform, unreliable with a multi-arch index digest).
docker run --rm \
	-e "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
	-e "KBUILD_BUILD_TIMESTAMP=@$SOURCE_DATE_EPOCH" \
	-e "KBUILD_BUILD_USER=$KBUILD_BUILD_USER" \
	-e "KBUILD_BUILD_HOST=$KBUILD_BUILD_HOST" \
	-e "VER=$VZ_KERNEL_VERSION" \
	-e "SYMS=$config_symbols" \
	-v "$tarball:/src/linux.tar.xz:ro" \
	-v "$(dirname "$OUT"):/out" \
	"$BUILDER_ARM64" bash -euo pipefail -c '
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq
		apt-get install -y -qq build-essential flex bison bc libssl-dev libelf-dev python3 xz-utils >/dev/null
		cd /src && tar xf linux.tar.xz && cd "linux-$VER"
		make -s defconfig
		# shellcheck disable=SC2086
		scripts/config $(for s in $SYMS; do printf -- "-e %s " "$s"; done)
		make -s olddefconfig
		# Assert every required symbol is actually =y (olddefconfig may drop one
		# whose dependency is unmet) — fail loud rather than ship a silent gap.
		for s in $SYMS; do
			grep -q "^CONFIG_${s}=y" .config || { echo "FATAL: CONFIG_${s} is not =y" >&2; exit 1; }
		done
		make -s -j"$(nproc)" Image
		cp arch/arm64/boot/Image "/out/'"$(basename "$OUT")"'"
	'

sha="$(shasum -a 256 "$OUT" | cut -d' ' -f1)"
echo "built vz kernel: $OUT"
echo "  sha256: $sha"
if [ -n "${VZ_KERNEL_IMAGE_SHA256:-}" ]; then
	if [ "$sha" != "$VZ_KERNEL_IMAGE_SHA256" ]; then
		echo "FATAL: kernel Image sha256 drift: got $sha, pinned $VZ_KERNEL_IMAGE_SHA256" >&2
		exit 1
	fi
	echo "  matches pinned VZ_KERNEL_IMAGE_SHA256 (reproducible)"
else
	echo "  (no VZ_KERNEL_IMAGE_SHA256 pinned yet — record this digest in pins.env)"
fi
