#!/usr/bin/env bash
# Build and install PINNED libkrunfw + libkrun (with networking) from source.
#
# WHY THIS EXISTS AT ALL. No Ubuntu release packages libkrun or libkrunfw, and
# there is no Linux tap, so every Linux install of the libkrun backend is a
# from-source build. Until MGIT-87 that recipe lived inline in one CI job and
# nowhere a developer could run it.
#
# WHY IT IS PINNED, which is the load-bearing part. The inline recipe cloned
# `main` of both repos. That is a moving target, and it moved: upstream libkrun
# after v1.19.4 turned `krun_set_workdir` into a stub returning -ENOTSUP for
# every non-aws-nitro build, so the first real Linux boot attempt died with
# "krun_set_workdir: libkrun error -95" — measured 2026-08-11, CI run
# 31482480288. Nothing about Linux was wrong; the library was simply a
# different library from the one macOS is validated against. Pinning to the
# versions Homebrew ships (libkrun 1.19.4 / libkrunfw 5.5.0) is what makes the
# Linux column comparable to the macOS one instead of a race against upstream.
#
# Usage:  sudo scripts/sandbox-image/build-libkrun.sh [PREFIX]
#         PREFIX defaults to /usr/local. A 64-bit build installs the libraries
#         into $PREFIX/lib64, so callers want:
#           export PKG_CONFIG_PATH=$PREFIX/lib64/pkgconfig
#           export LIBRARY_PATH=$PREFIX/lib64 LD_LIBRARY_PATH=$PREFIX/lib64
#
# Prerequisites (Ubuntu): build-essential flex bison libelf-dev
# python3-pyelftools bc pkg-config curl git ca-certificates patchelf, LLVM 18
# for bindgen, and a rustup toolchain WITH the musl target
# (`rustup target add $(uname -m)-unknown-linux-musl` — krun-init-blob's
# build.rs links krun-init as a static musl binary and panics without it).
#
# Refs: MGIT-87, MGIT-61.8, ADR-010
set -euo pipefail

prefix="${1:-/usr/local}"
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/sandbox-image/pins.env
. "$here/pins.env"

: "${LIBKRUN_VERSION:?pins.env must define LIBKRUN_VERSION}"
: "${LIBKRUNFW_VERSION:?pins.env must define LIBKRUNFW_VERSION}"

# bindgen needs a libclang; LLVM 18 is what this recipe was validated against.
# Both are overridable for a distro that puts them elsewhere.
export LIBCLANG_PATH="${LIBCLANG_PATH:-/usr/lib/llvm-18/lib}"
export BINDGEN_EXTRA_CLANG_ARGS="${BINDGEN_EXTRA_CLANG_ARGS:--resource-dir=/usr/lib/llvm-18/lib/clang/18}"

work="${LIBKRUN_BUILD_DIR:-$(mktemp -d)}"
jobs="$(nproc 2>/dev/null || echo 2)"

echo "== libkrunfw $LIBKRUNFW_VERSION (compiles a guest kernel; the slow step) =="
if [ ! -d "$work/libkrunfw" ]; then
	git clone --depth 1 --branch "$LIBKRUNFW_VERSION" \
		https://github.com/containers/libkrunfw.git "$work/libkrunfw"
fi
# `cd dir && make`, NOT `make -C dir`: libkrunfw recurses into the kernel build
# with $(MAKE) $(MAKEFLAGS), and with -C the propagated MAKEFLAGS made that
# recursive call literally invoke `make w -j...` ("No rule to make target 'w'").
(cd "$work/libkrunfw" && make -j"$jobs")
(cd "$work/libkrunfw" && make PREFIX="$prefix" install)
ldconfig

echo "== libkrun $LIBKRUN_VERSION (NET=1) =="
if [ ! -d "$work/libkrun" ]; then
	git clone --depth 1 --branch "$LIBKRUN_VERSION" \
		https://github.com/containers/libkrun.git "$work/libkrun"
fi
# NET=1 is not optional: without a virtio-net device libkrun falls back to TSI
# and the guest gets the host's network with no policy at all (ADR-010).
(cd "$work/libkrun" && make NET=1 -j"$jobs")
(cd "$work/libkrun" && make PREFIX="$prefix" install)
ldconfig

# --- Verify what was actually installed, rather than assuming ----------------
libdir="$prefix/lib64"
[ -d "$libdir" ] || libdir="$prefix/lib"
so="$(ls "$libdir"/libkrun.so.* 2>/dev/null | head -1 || true)"
[ -n "$so" ] || { echo "FATAL: no libkrun.so.* under $libdir after install" >&2; exit 1; }

# The networking symbols must be present. A libkrun built without them links
# and loads fine and only fails at VM configuration time, where the error names
# nothing useful.
if ! nm -D "$so" 2>/dev/null | grep -q 'krun_add_net_unixgram'; then
	echo "FATAL: $so was built WITHOUT networking (no krun_add_net_unixgram)." >&2
	echo "  Rebuild with 'make NET=1'. Without it libkrun falls back to TSI and" >&2
	echo "  the guest reaches the host network with no policy applied." >&2
	exit 1
fi

# krun_set_workdir is the call the version pin exists for: upstream stubbed it
# out after v1.19.4, and an unpinned build reintroduces the -ENOTSUP boot
# failure this script was written to prevent. Assert the symbol is real by
# checking it is not the only thing that changed — a stub is still exported, so
# the honest check is the version we resolved.
resolved="$(cd "$work/libkrun" && git describe --tags --always 2>/dev/null || echo unknown)"
if [ "$resolved" != "$LIBKRUN_VERSION" ]; then
	echo "FATAL: built libkrun $resolved, expected $LIBKRUN_VERSION" >&2
	exit 1
fi

echo "libkrun $LIBKRUN_VERSION + libkrunfw $LIBKRUNFW_VERSION installed under $prefix ($libdir), networking OK"
