#!/usr/bin/env bash
# build-darwin-libkrunfw.sh <prebuilt.tgz> <out-dir> <mgit-tag> — assemble the
# macOS libkrunfw tarball install.sh downloads in its opt-in step:
#
#   libkrunfw_<version>_darwin_arm64.tar.gz
#     libkrunfw.5.dylib, LICENSE-GPL-2.0-only, LICENSE-LGPL-2.1-only,
#     NOTICE.txt, SOURCE-OFFER.txt   (all at the top level)
#
# The library is built from upstream's prebuilt kernel image
# (LIBKRUNFW_PREBUILT_URL in scripts/sandbox-image/pins.env), whose sha256 is
# checked here before anything is unpacked. No patches of our own are applied.
# The caller fetches the asset (release.yml / e2e.yml, under guard-fetch.sh);
# this script reads only the file it is given. MAKE names the make to use.
# Must run on Apple silicon macOS: the dylib is compiled here.
# Refs: MGIT-306
set -euo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: build-darwin-libkrunfw.sh <prebuilt.tgz> <out-dir> <mgit-tag>" >&2
	exit 2
fi
prebuilt="$1" out="$2" tag="$3"

here="$(cd "$(dirname "$0")" && pwd)"
. "$here/../sandbox-image/pins.env"
ver="${LIBKRUNFW_VERSION#v}"
name="libkrunfw_${ver}_darwin_arm64.tar.gz"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

[ -f "$prebuilt" ] || { echo "no such prebuilt asset: $prebuilt" >&2; exit 1; }
got="$(sha256 "$prebuilt")"
if [ "$got" != "$LIBKRUNFW_PREBUILT_SHA256" ]; then
	echo "refusing: $prebuilt has sha256 $got, the pin says $LIBKRUNFW_PREBUILT_SHA256" >&2
	exit 1
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/build-darwin-libkrunfw.XXXXXX")" || { echo "cannot create a scratch directory" >&2; exit 1; }
case "$work" in "${TMPDIR:-/tmp}"/build-darwin-libkrunfw.*) ;; *) echo "refusing: unexpected scratch dir $work" >&2; exit 2 ;; esac
mkdir -p "$out"

tar -xzf "$prebuilt" -C "$work"
src="$work/libkrunfw"
[ -f "$src/Makefile" ] || { echo "the prebuilt asset has no libkrunfw/Makefile" >&2; exit 1; }

stage="$work/stage"
mkdir -p "$stage"
(cd "$src" && "${MAKE:-make}" && "${MAKE:-make}" PREFIX="$stage" install)
dylib="$stage/lib/libkrunfw.5.dylib"
[ -f "$dylib" ] && [ ! -L "$dylib" ] || { echo "the build left no $dylib" >&2; exit 1; }

pkg="$work/pkg"
mkdir -p "$pkg"
cp "$dylib" "$pkg/libkrunfw.5.dylib"
cp "$src/LICENSE-GPL-2.0-only" "$src/LICENSE-LGPL-2.1-only" "$pkg/"
cat >"$pkg/NOTICE.txt" <<NOTICE
libkrunfw $ver, from https://github.com/libkrun/libkrunfw, is a library that
bundles a Linux kernel (linux-$LIBKRUNFW_KERNEL_VERSION) as a storage format. Its library code is
distributed under LGPL-2.1-only, and the bundled kernel and the patches
applied to it under GPL-2.0-only; the license texts are LICENSE-LGPL-2.1-only
and LICENSE-GPL-2.0-only in this directory. libkrunfw is a separate library,
distributed separately from mgit.
NOTICE
bash "$here/libkrunfw-source-offer.sh" "$tag" >"$pkg/SOURCE-OFFER.txt"

# macOS tar would add an AppleDouble ._ entry for every file that carries an
# extended attribute; the tarball holds the five files and nothing else.
COPYFILE_DISABLE=1 tar -czf "$out/$name" -C "$pkg" libkrunfw.5.dylib LICENSE-GPL-2.0-only LICENSE-LGPL-2.1-only NOTICE.txt SOURCE-OFFER.txt
echo "built $out/$name ($(sha256 "$out/$name"))"
