#!/usr/bin/env bash
# Carry a patched libkrun in the macOS build; fixes MGIT-225.
#
# Usage (on an Apple Silicon Mac):
#   MGIT_BUILD_VERSION=0.7.0 scripts/release/build-darwin-sandboxd.sh <out-dir>
# Optional: MGIT_BUILD_COMMIT / MGIT_BUILD_DATE (default: goreleaser's own
# values for HEAD — ShortCommit and CommitDate), LIBKRUN_BUILD_DIR (an empty
# or absent directory to build in; default: a new temporary one), LIBCLANG_PATH.
# Needs: the Xcode command line tools, rustup's cargo, pkg-config, and ld.lld
# on PATH (`brew install lld`).
#
# Output: <out-dir>/{mgit-sandboxd, lib/libkrun.1.dylib, THIRD_PARTY/,
# buildinfo.json, MANIFEST.sha256}, consumed by the release's goreleaser
# through scripts/release/gobinary-prebuilt.sh.
# Refs: MGIT-259, MGIT-225
set -euo pipefail

out="${1:?usage: build-darwin-sandboxd.sh <out-dir>}"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
# shellcheck source=scripts/sandbox-image/pins.env
. "$root/scripts/sandbox-image/pins.env"

die() { echo "build-darwin-sandboxd: FATAL: $*" >&2; exit 1; }
[ "$(uname -s)/$(uname -m)" = Darwin/arm64 ] || die "this builds the darwin/arm64 daemon; run it on an Apple Silicon Mac"
: "${MGIT_BUILD_VERSION:?set MGIT_BUILD_VERSION (the release version, without the v)}"
: "${LIBKRUN_VERSION:?pins.env must define LIBKRUN_VERSION}"
: "${LIBKRUN_DARWIN_PATCH:?pins.env must define LIBKRUN_DARWIN_PATCH}"
: "${LIBKRUN_DARWIN_PATCH_SHA256:?pins.env must define LIBKRUN_DARWIN_PATCH_SHA256}"
for tool in go cargo pkg-config ld.lld install_name_tool codesign otool xcrun; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
done
export LIBCLANG_PATH="${LIBCLANG_PATH:-$(cd "$(dirname "$(xcrun --find clang)")/../lib" && pwd)}"
[ -f "$LIBCLANG_PATH/libclang.dylib" ] || die "no libclang.dylib in LIBCLANG_PATH ($LIBCLANG_PATH)"

# The same two git questions goreleaser asks for {{.ShortCommit}} and
# {{.CommitDate}}, so the daemon's stamp and every other binary's agree.
commit="${MGIT_BUILD_COMMIT:-$(git -C "$root" show --format=%h HEAD --quiet)}"
date="${MGIT_BUILD_DATE:-$(date -u -r "$(git -C "$root" show --format=%ct HEAD --quiet)" +%Y-%m-%dT%H:%M:%SZ)}"

work="${LIBKRUN_BUILD_DIR:-$(mktemp -d)}"
src="$work/libkrun"
prefix="$work/prefix"
[ ! -e "$src" ] || die "$src already exists; point LIBKRUN_BUILD_DIR at an empty directory"
[ ! -e "$prefix" ] || die "$prefix already exists; point LIBKRUN_BUILD_DIR at an empty directory"
jobs="$(sysctl -n hw.ncpu 2>/dev/null || echo 2)"
GUARD="$root/scripts/ci/guard-fetch.sh"
BOUND="${MGIT_LIBKRUN_BUILD_TIMEOUT:-1800}"

rm -rf "$out"
mkdir -p "$out/lib" "$out/THIRD_PARTY"

echo "== 1/6 libkrun $LIBKRUN_VERSION =="
"$GUARD" -t "$BOUND" -l libkrun-darwin-clone -c "rm -rf '$src'" -- \
	git clone --depth 1 --branch "$LIBKRUN_VERSION" https://github.com/containers/libkrun.git "$src"
resolved="$(git -C "$src" describe --tags --always)"
[ "$resolved" = "$LIBKRUN_VERSION" ] || die "cloned libkrun $resolved, expected $LIBKRUN_VERSION"
bash "$here/apply-libkrun-patch.sh" "$src" "$here/../sandbox-image/$LIBKRUN_DARWIN_PATCH" "$LIBKRUN_DARWIN_PATCH_SHA256"

echo "== 2/6 the Linux sysroot libkrun's Makefile fetches for its guest init =="
"$GUARD" -t 600 -l libkrun-darwin-sysroot -c "rm -rf '$src/linux-sysroot'" -- \
	sh -c "cd '$src' && make linux-sysroot/.sysroot_ready"

echo "== 3/6 build (NET=1, TIMESYNC=1) and install into $prefix =="
export TIMESYNC=1
"$GUARD" -t "$BOUND" -l libkrun-darwin-build \
	-c none:'cargo verifies every downloaded crate against its checksum and discards a partial, so no corrupt artifact survives to satisfy a later existence check; the build tree is kept deliberately, which is what makes a retry cost the fetch rather than the compile' -- \
	sh -c "cd '$src' && make NET=1 TIMESYNC=1 FEATURE_FLAGS='--features net,krun-display/bindgen_clang_runtime,krun-input/bindgen_clang_runtime' -j'$jobs'"
(cd "$src" && make PREFIX="$prefix" install)
krun="$prefix/lib/libkrun.${LIBKRUN_VERSION#v}.dylib"
[ -f "$krun" ] || die "no $krun after the install"

echo "== 4/6 lib/libkrun.1.dylib =="
install_name_tool -id @rpath/libkrun.1.dylib "$krun"
codesign --force --sign - "$krun"
cp "$krun" "$out/lib/libkrun.1.dylib"
chmod 0644 "$out/lib/libkrun.1.dylib"

echo "== 5/6 mgit-sandboxd, linked against it and signed =="
pkg="github.com/hyper-swe/mgit/internal/buildinfo"
(
	cd "$root"
	# Single quotes: @executable_path is for the dynamic loader, never the shell.
	CGO_ENABLED=1 PKG_CONFIG_PATH="$prefix/lib/pkgconfig" PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" \
		CGO_LDFLAGS='-Wl,-rpath,@executable_path/lib -Wl,-rpath,@executable_path/../lib/mgit' \
		go build -trimpath \
		-ldflags "-s -w -X $pkg.version=$MGIT_BUILD_VERSION -X $pkg.commit=$commit -X $pkg.date=$date -X github.com/hyper-swe/mgit/internal/sandboxd/backend/libkrun.bundled=1" \
		-o "$out/mgit-sandboxd" ./cmd/mgit-sandboxd/
)
codesign --force --sign - --entitlements "$root/build/darwin/vz.entitlements" "$out/mgit-sandboxd"

echo "== 6/6 license texts, the source notice, and the verifier =="
lic() { [ -f "$1" ] || die "expected license file $1 is missing (did upstream move it?)"; cp "$1" "$out/THIRD_PARTY/$2"; }
lic "$src/LICENSE" "libkrun-LICENSE"
lic "$src/AUTHORS" "libkrun-AUTHORS"
cat >"$out/THIRD_PARTY/SOURCES.txt" <<EOF
mgit-sandboxd in this archive links one library shipped beside it in lib/:

  libkrun.1.dylib   libkrun $LIBKRUN_VERSION (Apache-2.0), https://github.com/containers/libkrun,
                    with scripts/sandbox-image/$LIBKRUN_DARWIN_PATCH of the mgit repository
                    applied (sha256 $LIBKRUN_DARWIN_PATCH_SHA256)

It was built by scripts/release/build-darwin-sandboxd.sh in the mgit
repository at commit $commit. The license texts are the other files in this
directory.
EOF
bash "$here/verify-darwin-sandboxd.sh" "$out"

printf '{"version":"%s","commit":"%s","date":"%s","goarch":"arm64","libkrun":"%s","libkrun_patch_sha256":"%s"}\n' \
	"$MGIT_BUILD_VERSION" "$commit" "$date" "$LIBKRUN_VERSION" "$LIBKRUN_DARWIN_PATCH_SHA256" >"$out/buildinfo.json"
(cd "$out" && find . -type f ! -name MANIFEST.sha256 | LC_ALL=C sort | xargs shasum -a 256 >MANIFEST.sha256)
echo "build-darwin-sandboxd: OK — $out (darwin/arm64, mgit $MGIT_BUILD_VERSION $commit $date)"
