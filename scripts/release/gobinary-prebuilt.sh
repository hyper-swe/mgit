#!/usr/bin/env bash
# goreleaser's build `tool` (formerly `gobinary`) for the mgit-sandboxd-linux
# build: hands goreleaser
# the libkrun-linked daemon that scripts/release/build-linux-sandboxd.sh built
# in ubuntu:20.04, instead of compiling one here.
#
# WHY. The Linux daemon links libkrun through cgo, and the release runs on a
# macOS runner (the darwin daemon must be built and signed natively there);
# open-source goreleaser cannot cross-compile a cgo Linux binary or take a
# prebuilt one. It does let a build name its own go tool, and it only ever
# invokes that tool as `<gobinary> build … -o <out> <main>` — so this script
# answers `build` by copying the prebuilt daemon, its lib/ and its license
# texts to where goreleaser expects them. Everything downstream (archives,
# checksums, signing) is goreleaser's, unchanged. Refs: MGIT-229, ADR-016
#
# WHAT IT REFUSES, loudly, so a release cannot ship the wrong daemon:
#   - no prebuilt for the target (MGIT_LINUX_PREBUILT/linux_<arch>): it never
#     falls back to compiling — a CGO-free build here would silently ship the
#     firecracker daemon the Linux loop cannot use;
#   - a prebuilt whose files do not match its MANIFEST.sha256, before or
#     after the copy (verified in, verified out);
#   - on a real release, a daemon stamped with a version, commit or date other
#     than the ones goreleaser stamps into mgit — the two binaries of one
#     archive must report one build. A snapshot's version is goreleaser's own
#     invention, so a snapshot says it did not compare, and why.
#
# Env: MGIT_LINUX_PREBUILT  directory holding linux_amd64/ and/or linux_arm64/
#      GOOS, GOARCH         set by goreleaser for the target
set -euo pipefail

say() { echo "gobinary-prebuilt: $*" >&2; }
die() { say "FATAL: $*"; exit 1; }

if [ "${1:-}" != build ]; then
	die "goreleaser asked for '${*}', and this stand-in only answers 'build' (it never compiles)"
fi
shift
out="" ldflags="" prev=""
for a in "$@"; do
	case "$prev" in -o) out="$a" ;; -ldflags) ldflags="$a" ;; esac
	case "$a" in -ldflags=*) ldflags="${a#-ldflags=}" ;; esac
	prev="$a"
done
[ -n "$out" ] || die "no -o in: $*"
if [ "$(basename "$out")" != mgit-sandboxd ]; then
	die "goreleaser asked for $(basename "$out"); this stand-in serves only mgit-sandboxd"
fi
[ "${GOOS:-}" = linux ] || die "the prebuilt daemon is Linux-only; asked for GOOS=${GOOS:-unset}"
[ -n "${MGIT_LINUX_PREBUILT:-}" ] || die "MGIT_LINUX_PREBUILT is not set: build the daemon with scripts/release/build-linux-sandboxd.sh (in ubuntu:20.04) and point MGIT_LINUX_PREBUILT at the directory holding linux_<arch>/"
src="$MGIT_LINUX_PREBUILT/linux_${GOARCH:?GOARCH unset}"
[ -f "$src/mgit-sandboxd" ] || die "no prebuilt daemon at $src/mgit-sandboxd"
[ -f "$src/MANIFEST.sha256" ] || die "no MANIFEST.sha256 in $src"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
# check_manifest <root>: every file named in the prebuilt's manifest exists
# under <root> with the recorded digest.
check_manifest() {
	local root="$1" want path got
	while read -r want path; do
		path="${path#./}"
		[ -f "$root/$path" ] || die "$root/$path is missing (named in $src/MANIFEST.sha256)"
		got="$(sha256 "$root/$path")"
		[ "$got" = "$want" ] || die "$root/$path has digest $got, not the manifest's $want"
	done <"$src/MANIFEST.sha256"
}
check_manifest "$src"

# stamp <key>: the -X value goreleaser passed for internal/buildinfo.<key>.
stamp() { printf '%s\n' "$ldflags" | tr ' ' '\n' | sed -n "s|^github.com/hyper-swe/mgit/internal/buildinfo\.$1=||p" | head -1; }
# built <key>: the value the prebuilt daemon was stamped with.
built() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" "$src/buildinfo.json"; }
want_version="$(stamp version)"
if [ -z "$want_version" ]; then
	die "goreleaser passed no buildinfo.version in -ldflags ($ldflags); cannot tell which build this is"
fi
case "$want_version" in
*SNAPSHOT*)
	say "snapshot ($want_version): stamps not compared; the daemon says $(built version) $(built commit) $(built date)"
	;;
*)
	for key in version commit date; do
		[ "$(stamp "$key")" = "$(built "$key")" ] ||
			die "the prebuilt daemon was stamped $key=$(built "$key") but this release stamps $key=$(stamp "$key"); rebuild it from this commit — one archive must not carry two builds"
	done
	;;
esac

dest="$(dirname "$out")"
mkdir -p "$dest/lib" "$dest/THIRD_PARTY"
cp "$src/mgit-sandboxd" "$out"
chmod 0755 "$out"
cp "$src"/lib/* "$dest/lib/"
cp "$src"/THIRD_PARTY/* "$dest/THIRD_PARTY/"
cp "$src/buildinfo.json" "$dest/"
check_manifest "$dest"
say "linux/$GOARCH: $out <- $src (manifest verified in and out)"
