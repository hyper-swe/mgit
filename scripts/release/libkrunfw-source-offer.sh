#!/usr/bin/env bash
# libkrunfw-source-offer.sh <mgit-tag> — print the written source offer that
# travels inside the macOS libkrunfw tarball (SOURCE-OFFER.txt), for the
# libkrunfw version pinned in scripts/sandbox-image/pins.env.
#
# The tarball holds a library built from upstream's prebuilt kernel image and
# applies no patches of its own, so the offer says that, points at the build
# steps at the release's own tag, and names the release that holds the source
# beside the tarball. The same wording stands in the tap formula's offer.
# Refs: MGIT-306
set -euo pipefail

tag="${1:-}"
[ -n "$tag" ] || { echo "usage: libkrunfw-source-offer.sh <mgit-tag>" >&2; exit 2; }

here="$(cd "$(dirname "$0")" && pwd)"
. "$here/../sandbox-image/pins.env"
ver="${LIBKRUNFW_VERSION#v}"
rel="https://github.com/hyper-swe/mgit/releases/tag/$tag"

cat <<OFFER
libkrunfw $ver is distributed under LGPL-2.1-only for the library code
and GPL-2.0-only for the bundled Linux kernel (linux-$LIBKRUNFW_KERNEL_VERSION) and the
patches applied to it. The complete corresponding source is available
to anyone, free of charge:

  1. Upstream libkrunfw source (library code, kernel patches, kernel
     configuration and build scripts), tag $LIBKRUNFW_VERSION:
     https://github.com/libkrun/libkrunfw/releases/tag/$LIBKRUNFW_VERSION
  2. The Linux kernel source the image was built from:
     $LIBKRUNFW_KERNEL_URL
  3. We apply no patches of our own. This library was built from
     upstream's prebuilt kernel image (libkrunfw-prebuilt-aarch64.tgz,
     object code, sha256 $LIBKRUNFW_PREBUILT_SHA256) by the build steps in:
     https://github.com/hyper-swe/mgit/blob/$tag/scripts/release/build-darwin-libkrunfw.sh
  4. A copy of items 1 and 2, and of the prebuilt kernel image, kept by us
     beside this library's binaries, with checksums:
     $rel
     libkrunfw-$LIBKRUNFW_VERSION-kernel-linux-$LIBKRUNFW_KERNEL_VERSION.tar.xz,
     libkrunfw-$LIBKRUNFW_VERSION-source.tar.gz,
     libkrunfw-$LIBKRUNFW_VERSION-prebuilt-aarch64.tgz (object code, for reference)

This offer is valid for at least three years from the date the binary
was last distributed, and for as long as the binary is offered; the
hosted source is kept for the life of the release.
If any link above stops working, write to swe@hyperswe.com and we will
send the complete corresponding source, at no charge beyond the cost of
physically providing it. This offer is open to anyone who has this binary.
OFFER
