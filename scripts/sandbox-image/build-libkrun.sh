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

# Every network step below goes through the ONE guard: bounded retry, a
# per-attempt timeout, and a precondition restore between attempts. This file
# is where clause 3 was learned the hard way, so it is also where the
# generalized version earns its keep. Refs: MGIT-143, MGIT-119
GUARD="$here/../ci/guard-fetch.sh"

# -t 1800 for every step here. The MGIT-143 rule is 4x the slowest SUCCESSFUL
# observation of the call site; the enclosing CI step ("Build the PINNED
# libkrunfw + libkrun") has a worst success of 905s across n=195, and 4x that
# (3620s) is brought to 1800s by the rule's cap so three attempts still fit the
# job budget. The cap is safe HERE specifically because this step's duration is
# dominated by a kernel COMPILE rather than by a transfer: 1800s is still 2x
# the slowest run ever recorded and ~100x the ~141 MB download it is really
# guarding.
BOUND="${MGIT_LIBKRUN_BUILD_TIMEOUT:-1800}"

# ---------------------------------------------------------------------------
# The pinned kernel tarball, cached across runs.
#
# libkrunfw's build downloads a ~145 MB kernel tarball from a third party
# before it compiles anything, on EVERY run. That put a transfer to a rented
# runner on the critical path of the whole repository: a network hiccup there
# blocked an unrelated PR three times in a row, with the guard doing everything
# a guard can and the fetch simply unable to succeed.
#
# The tarball is PINNED, so its identity is its content, and content-addressed
# things are cacheable by construction — the same argument that moved guest
# bases out of repositories (MGIT-147), applied to CI.
#
# A cached copy is VERIFIED, never trusted: the digest is checked on the way in
# and on the way out, so a truncated or tampered cache entry is discarded and
# refetched rather than compiled. Caching must not weaken the thing it speeds
# up. Refs: MGIT-163, MGIT-147
# ---------------------------------------------------------------------------

# sha256_of prints the SHA-256 of a file, using whichever tool this host has.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# kernel_tarball_name is the file libkrunfw's Makefile expects to find.
kernel_tarball_name() { echo "linux-$LIBKRUNFW_KERNEL_VERSION.tar.xz"; }

# seed_kernel_tarball places a verified cached tarball where the build looks,
# so the download is skipped. A miss is silent and harmless: the build fetches,
# exactly as it always did.
seed_kernel_tarball() {
	src_dir="${MGIT_LIBKRUN_CACHE:-}"
	[ -n "$src_dir" ] || return 0
	name="$(kernel_tarball_name)"
	src="$src_dir/$name"
	[ -f "$src" ] || return 0

	got="$(sha256_of "$src")"
	if [ "$got" != "$LIBKRUNFW_KERNEL_SHA256" ]; then
		echo "libkrun cache: $name failed its digest check (got $got); discarding, the build will refetch"
		rm -f "$src"
		return 0
	fi
	mkdir -p "$1/tarballs"
	cp "$src" "$1/tarballs/$name"
	echo "libkrun cache: seeded $name from cache (digest verified); skipping the ~145 MB download"
}

# save_kernel_tarball copies a verified tarball INTO the cache after a build.
#
# The digest is re-checked here rather than assumed: this runs after a build
# that may have downloaded the file itself, and a cache is only worth having if
# what it holds was checked before it went in.
save_kernel_tarball() {
	dst_dir="${MGIT_LIBKRUN_CACHE:-}"
	[ -n "$dst_dir" ] || return 0
	name="$(kernel_tarball_name)"
	built="$1/tarballs/$name"
	[ -f "$built" ] || return 0
	[ -f "$dst_dir/$name" ] && return 0

	got="$(sha256_of "$built")"
	if [ "$got" != "$LIBKRUNFW_KERNEL_SHA256" ]; then
		echo "libkrun cache: refusing to cache $name — digest $got does not match the pin"
		return 0
	fi
	mkdir -p "$dst_dir"
	cp "$built" "$dst_dir/$name.tmp" && mv "$dst_dir/$name.tmp" "$dst_dir/$name"
	echo "libkrun cache: stored $name for later runs"
}

echo "== libkrunfw $LIBKRUNFW_VERSION (compiles a guest kernel; the slow step) =="
# The `[ ! -d ]` skip below is the SAME existence-check trap that made the
# kernel-tarball retry fail three times identically: a clone that dies part-way
# leaves the directory behind, and every later attempt would then skip the
# clone and build from a half-populated tree. So the restore removes it, and
# the two are read together rather than separately.
if [ ! -d "$work/libkrunfw" ]; then
	"$GUARD" -t "$BOUND" -l libkrunfw-clone -c "rm -rf '$work/libkrunfw'" -- \
		git clone --depth 1 --branch "$LIBKRUNFW_VERSION" \
		https://github.com/containers/libkrunfw.git "$work/libkrunfw"
fi
# `cd dir && make`, NOT `make -C dir`: libkrunfw recurses into the kernel build
# with $(MAKE) $(MAKEFLAGS), and with -C the propagated MAKEFLAGS made that
# recursive call literally invoke `make w -j...` ("No rule to make target 'w'").
#
# This build is guarded because its first act is to download a ~141 MB kernel
# tarball from a third-party host, and a truncated transfer there is not a
# defect in this repository. Observed twice in a row on 2026-08-19:
#   curl: (18) transfer closed with 30852240 bytes remaining to read
#   curl: (18) transfer closed with 124175504 bytes remaining to read
#
# THE RESTORE IS THE POINT, and it is the clause this file taught the rest of
# the repo. The first version of this retry said make would refetch a partial
# download. It does not: a truncated tarball satisfies the download target's
# existence check, so every later attempt re-extracted the SAME corrupt file
# and died identically at the extract step -- three deterministic failures
# wearing the costume of a transient, announced by a message that said "treat
# this as a real outage, not a flake". Loud, well-worded, and honest about the
# wrong thing. Discarding the tarball is what makes attempt 2 an attempt.
# Already-built objects are kept, so a real retry costs the download, not the
# compile. Refs: MGIT-143 clause 3, MGIT-119
seed_kernel_tarball "$work/libkrunfw"
"$GUARD" -t "$BOUND" -l libkrunfw-build \
	-c "rm -f '$work/libkrunfw'/tarballs/*.tar.*" -- \
	sh -c "cd '$work/libkrunfw' && make -j'$jobs'"
save_kernel_tarball "$work/libkrunfw"
(cd "$work/libkrunfw" && make PREFIX="$prefix" install)
ldconfig

echo "== libkrun $LIBKRUN_VERSION (NET=1) =="
# Same existence-check trap as libkrunfw's clone above; same restore.
if [ ! -d "$work/libkrun" ]; then
	"$GUARD" -t "$BOUND" -l libkrun-clone -c "rm -rf '$work/libkrun'" -- \
		git clone --depth 1 --branch "$LIBKRUN_VERSION" \
		https://github.com/containers/libkrun.git "$work/libkrun"
fi
# NET=1 is not optional: without a virtio-net device libkrun falls back to TSI
# and the guest gets the host's network with no policy at all (ADR-010).
#
# Guarded because this build resolves and downloads crates from crates.io --
# the same third-party-transfer exposure as the tarball above, just wearing
# cargo's clothes.
"$GUARD" -t "$BOUND" -l libkrun-build \
	-c none:'cargo verifies every downloaded crate against its checksum and discards a partial, so no corrupt artifact survives to satisfy a later existence check; the build tree is kept deliberately, which is what makes a retry cost the fetch rather than the compile' -- \
	sh -c "cd '$work/libkrun' && make NET=1 -j'$jobs'"
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
