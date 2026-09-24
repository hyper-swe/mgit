#!/usr/bin/env bash
# The ONE assembler for the Linux release's sandbox daemon: libkrun + libkrunfw
# built from their pinned sources, bundled beside a libkrun-linked
# mgit-sandboxd, with the license texts that bundling obliges, verified before
# anything leaves this script.
#
# WHY A BUNDLE. No Ubuntu release packages libkrun or libkrunfw, so a stock
# Linux host has no way to get the only Linux backend that serves the agent
# loop (sync and export; firecracker refuses both by design). The Linux
# archives therefore carry the two libraries themselves, and a stock host needs
# nothing but /dev/kvm. Built in ubuntu:20.04, so the glibc floor is 2.31.
# Refs: MGIT-229, hyper-swe/mgit#12, ADR-016
#
# WHY THE RUN PATHS ARE DT_RPATH, NOT DT_RUNPATH. libkrun dlopen()s
# libkrunfw.so.5 BY LEAF NAME from its own code. That search consults
# libkrun's own DT_RPATH, which comes BEFORE LD_LIBRARY_PATH, and the VM
# child's environment adds the usual system prefixes to LD_LIBRARY_PATH
# whenever they hold a libkrunfw. With a DT_RUNPATH a system libkrunfw of
# another version would win over the bundled one, which is exactly the version
# skew the pins exist to prevent. The daemon's own DT_RPATH covers both
# layouts a user can have: the extracted archive ($ORIGIN/lib) and install.sh's
# ($ORIGIN/../lib/mgit).
#
# Usage (as root, inside ubuntu:20.04 with the build prerequisites installed —
# see scripts/release/linux-build-prereqs.sh):
#   MGIT_BUILD_VERSION=0.7.0 scripts/release/build-linux-sandboxd.sh <out-dir>
# Optional: MGIT_BUILD_COMMIT / MGIT_BUILD_DATE (default: goreleaser's own
# values for HEAD — ShortCommit and CommitDate), MGIT_LIBKRUN_PREFIX (where the
# libraries are built, default /opt/mgit-libkrun), MGIT_LIBKRUN_CACHE (the
# verified kernel-tarball cache build-libkrun.sh already honours).
#
# Output: <out-dir>/{mgit-sandboxd, lib/, THIRD_PARTY/, buildinfo.json,
# MANIFEST.sha256}. The release's goreleaser consumes it through
# scripts/release/gobinary-prebuilt.sh.
set -euo pipefail

out="${1:?usage: build-linux-sandboxd.sh <out-dir>}"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
# shellcheck source=scripts/sandbox-image/pins.env
. "$root/scripts/sandbox-image/pins.env"

GLIBC_FLOOR="2.31"
prefix="${MGIT_LIBKRUN_PREFIX:-/opt/mgit-libkrun}"
work="${LIBKRUN_BUILD_DIR:-$(mktemp -d)}"

die() { echo "build-linux-sandboxd: FATAL: $*" >&2; exit 1; }
[ "$(uname -s)" = Linux ] || die "this builds the LINUX daemon; run it inside ubuntu:20.04"
case "$(uname -m)" in
x86_64) goarch=amd64 ;;
aarch64) goarch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac
: "${MGIT_BUILD_VERSION:?set MGIT_BUILD_VERSION (the release version, without the v)}"
git -C "$root" config --global --add safe.directory "$root" >/dev/null 2>&1 || true
# The same two git questions goreleaser asks for {{.ShortCommit}} and
# {{.CommitDate}}, so the daemon's stamp and every other binary's agree.
commit="${MGIT_BUILD_COMMIT:-$(git -C "$root" show --format=%h HEAD --quiet)}"
date="${MGIT_BUILD_DATE:-$(date -u -d "@$(git -C "$root" show --format=%ct HEAD --quiet)" +%Y-%m-%dT%H:%M:%SZ)}"

rm -rf "$out"
mkdir -p "$out/lib" "$out/THIRD_PARTY"

echo "== 1/5 libkrun $LIBKRUN_VERSION + libkrunfw $LIBKRUNFW_VERSION into $prefix =="
LIBKRUN_BUILD_DIR="$work" bash "$root/scripts/sandbox-image/build-libkrun.sh" "$prefix"
libdir="$prefix/lib64"
[ -d "$libdir" ] || libdir="$prefix/lib"

echo "== 2/5 stage the libraries under their SONAMEs =="
soname() { readelf -d "$1" | sed -n 's/.*(SONAME).*\[\(.*\)\].*/\1/p'; }
for lib in libkrun libkrunfw; do
	real="$(readlink -f "$libdir/$lib.so")"
	[ -f "$real" ] || die "$lib.so is not under $libdir after the build"
	name="$(soname "$real")"
	[ -n "$name" ] || die "$real carries no SONAME"
	cp "$real" "$out/lib/$name"
	chmod 0644 "$out/lib/$name"
	echo "  $out/lib/$name  (from $real)"
done
krun_so="$(soname "$(readlink -f "$libdir/libkrun.so")")"
krunfw_so="$(soname "$(readlink -f "$libdir/libkrunfw.so")")"
patchelf --force-rpath --set-rpath '$ORIGIN' "$out/lib/$krun_so"

echo "== 3/5 mgit-sandboxd, linked against the bundle =="
pkg="github.com/hyper-swe/mgit/internal/buildinfo"
(
	cd "$root"
	# Single quotes: $ORIGIN is for the dynamic loader, never the shell.
	CGO_ENABLED=1 PKG_CONFIG_PATH="$libdir/pkgconfig" \
		CGO_LDFLAGS='-Wl,--disable-new-dtags -Wl,-rpath,$ORIGIN/lib -Wl,-rpath,$ORIGIN/../lib/mgit' \
		go build -tags libkrun -trimpath \
		-ldflags "-s -w -X $pkg.version=$MGIT_BUILD_VERSION -X $pkg.commit=$commit -X $pkg.date=$date" \
		-o "$out/mgit-sandboxd" ./cmd/mgit-sandboxd/
)

echo "== 4/5 license texts and the source notice =="
lic() { [ -f "$1" ] || die "expected license file $1 is missing (did upstream move it?)"; cp "$1" "$out/THIRD_PARTY/$2"; }
lic "$work/libkrun/LICENSE" "libkrun-LICENSE"
lic "$work/libkrun/AUTHORS" "libkrun-AUTHORS"
lic "$work/libkrunfw/LICENSE-GPL-2.0-only" "libkrunfw-LICENSE-GPL-2.0-only"
lic "$work/libkrunfw/LICENSE-LGPL-2.1-only" "libkrunfw-LICENSE-LGPL-2.1-only"
cat >"$out/THIRD_PARTY/SOURCES.txt" <<EOF
mgit-sandboxd in this archive links two libraries shipped beside it in lib/:

  $krun_so     libkrun $LIBKRUN_VERSION (Apache-2.0), https://github.com/containers/libkrun
  $krunfw_so   libkrunfw $LIBKRUNFW_VERSION, https://github.com/containers/libkrunfw
               its glue code is LGPL-2.1-only; it carries a Linux $LIBKRUNFW_KERNEL_VERSION
               kernel (GPL-2.0-only) that runs only inside the guest microVM

The corresponding source of both is published with every mgit release that
ships them, as release assets:

  libkrunfw-$LIBKRUNFW_VERSION-kernel-linux-$LIBKRUNFW_KERNEL_VERSION.tar.xz
      the kernel source, byte-identical to kernel.org's
      (sha256 $LIBKRUNFW_KERNEL_SHA256)
  libkrunfw-$LIBKRUNFW_VERSION-source.tar.gz
      libkrunfw at $LIBKRUNFW_VERSION: the kernel patches, the kernel
      configuration and the build scripts applied to that kernel
  libkrun-$LIBKRUN_VERSION-source.tar.gz
      libkrun at $LIBKRUN_VERSION

They were built by scripts/release/build-linux-sandboxd.sh in the mgit
repository at commit $commit, in an ubuntu:20.04 container.
The license texts are the other files in this directory.
EOF

echo "== 5/5 verify the bundle =="
bash "$here/verify-linux-sandboxd.sh" "$out" "$prefix" "$GLIBC_FLOOR"

printf '{"version":"%s","commit":"%s","date":"%s","goarch":"%s","libkrun":"%s","libkrunfw":"%s"}\n' \
	"$MGIT_BUILD_VERSION" "$commit" "$date" "$goarch" "$LIBKRUN_VERSION" "$LIBKRUNFW_VERSION" >"$out/buildinfo.json"
(cd "$out" && find . -type f ! -name MANIFEST.sha256 | LC_ALL=C sort | xargs sha256sum >MANIFEST.sha256)
echo "build-linux-sandboxd: OK — $out (linux/$goarch, mgit $MGIT_BUILD_VERSION $commit $date)"
