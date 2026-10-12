#!/usr/bin/env bash
# Self-test for scripts/release/gobinary-prebuilt.sh against fixture
# prebuilts: the copy it must make, and every refusal it owes. Each refusal is
# a case that must FAIL; the copy is the positive control that proves the
# fixtures are good enough for a refusal to mean something.
# Refs: MGIT-229, MGIT-259
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
shim="$here/gobinary-prebuilt.sh"
T="$(mktemp -d "${TMPDIR:-/tmp}/gobinary-prebuilt-selftest.XXXXXX")"
case "$T" in "${TMPDIR:-/tmp}"/gobinary-prebuilt-selftest.*) ;; *) echo "refusing: unexpected scratch dir $T" >&2; exit 2 ;; esac
trap 'rm -rf "$T"' EXIT
failures=0
X=github.com/hyper-swe/mgit/internal/buildinfo

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
# fixture <dir> <version> <commit> <date>: a prebuilt as the assembler lays it out.
fixture() {
	local d="$1/linux_amd64"
	mkdir -p "$d/lib" "$d/THIRD_PARTY"
	printf 'daemon\n' >"$d/mgit-sandboxd"
	printf 'krun\n' >"$d/lib/libkrun.so.1"
	printf 'krunfw\n' >"$d/lib/libkrunfw.so.5"
	printf 'apache\n' >"$d/THIRD_PARTY/libkrun-LICENSE"
	printf '{"version":"%s","commit":"%s","date":"%s","goarch":"amd64"}\n' "$2" "$3" "$4" >"$d/buildinfo.json"
	(cd "$d" && for f in buildinfo.json mgit-sandboxd lib/libkrun.so.1 lib/libkrunfw.so.5 THIRD_PARTY/libkrun-LICENSE; do
		printf '%s  ./%s\n' "$(sha256 "$f")" "$f"
	done >MANIFEST.sha256)
}
# darwin_fixture <dir> <version> <commit> <date>: a darwin/arm64 prebuilt.
darwin_fixture() {
	local d="$1/darwin_arm64"
	mkdir -p "$d/lib" "$d/THIRD_PARTY"
	printf 'daemon\n' >"$d/mgit-sandboxd"
	printf 'krun\n' >"$d/lib/libkrun.1.dylib"
	printf 'apache\n' >"$d/THIRD_PARTY/libkrun-LICENSE"
	printf '{"version":"%s","commit":"%s","date":"%s","goarch":"arm64"}\n' "$2" "$3" "$4" >"$d/buildinfo.json"
	(cd "$d" && for f in buildinfo.json mgit-sandboxd lib/libkrun.1.dylib THIRD_PARTY/libkrun-LICENSE; do
		printf '%s  ./%s\n' "$(sha256 "$f")" "$f"
	done >MANIFEST.sha256)
}
# run <prebuilt-root> <goos> <version> <commit> <date> <out>
run() {
	env MGIT_LINUX_PREBUILT="$1" GOOS="$2" GOARCH=amd64 bash "$shim" build -trimpath \
		"-ldflags=-s -w -X $X.version=$3 -X $X.commit=$4 -X $X.date=$5" -o "$6" ./cmd/mgit-sandboxd/ 2>&1
}
# run_darwin <prebuilt-root> <goarch> <version> <commit> <date> <out>
run_darwin() {
	env -u MGIT_LINUX_PREBUILT MGIT_DARWIN_PREBUILT="$1" GOOS=darwin GOARCH="$2" bash "$shim" build -trimpath \
		"-ldflags=-s -w -X $X.version=$3 -X $X.commit=$4 -X $X.date=$5" -o "$6" ./cmd/mgit-sandboxd/ 2>&1
}
expect() { # <label> <want: ok|fail> <needle> <output> <rc>
	local label="$1" want="$2" needle="$3" out="$4" rc="$5"
	if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = fail ] && [ "$rc" -ne 0 ]; }; then
		case "$out" in *"$needle"*) echo "  pass  $label"; return ;; esac
	fi
	echo "  FAIL  $label (rc=$rc, wanted $want with \"$needle\"): $out"
	failures=$((failures + 1))
}

good="$T/good"; fixture "$good" 0.7.0 abc1234 2026-09-24T00:00:00Z
o="$T/o1/mgit-sandboxd_linux_amd64_v1/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$good" linux 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "release stamps match: the daemon, lib/ and THIRD_PARTY/ are placed" ok "manifest verified in and out" "$out" "$rc"
[ -f "$(dirname "$o")/lib/libkrunfw.so.5" ] && [ -f "$(dirname "$o")/THIRD_PARTY/libkrun-LICENSE" ] ||
	{ echo "  FAIL  the positive case did not place lib/ and THIRD_PARTY/"; failures=$((failures + 1)); }

o="$T/o2/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$good" linux 0.7.1-SNAPSHOT-abc1234 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "a snapshot copies and says it did not compare" ok "stamps not compared" "$out" "$rc"

o="$T/o3/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$good" linux 0.7.0 def5678 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "a release whose commit differs from the daemon's is refused" fail "stamped commit=abc1234 but this release stamps commit=def5678" "$out" "$rc"
[ -e "$o" ] && { echo "  FAIL  the refused case still wrote $o"; failures=$((failures + 1)); }

o="$T/o4/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$good" linux 0.7.0 abc1234 2026-09-25T00:00:00Z "$o")"; rc=$?
expect "a release whose date differs is refused" fail "stamped date=" "$out" "$rc"

tampered="$T/tampered"; fixture "$tampered" 0.7.0 abc1234 2026-09-24T00:00:00Z
printf 'other bytes\n' >"$tampered/linux_amd64/lib/libkrunfw.so.5"
o="$T/o5/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$tampered" linux 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "a prebuilt that does not match its manifest is refused" fail "not the manifest's" "$out" "$rc"

o="$T/o6/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$T/nowhere" linux 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "no prebuilt for the target: refused, never compiled" fail "no prebuilt daemon at" "$out" "$rc"

out="$(env -u MGIT_LINUX_PREBUILT GOOS=linux GOARCH=amd64 bash "$shim" build -o "$T/o7/mgit-sandboxd" . 2>&1)"; rc=$?
expect "MGIT_LINUX_PREBUILT unset: refused with the build instructions" fail "build-linux-sandboxd.sh" "$out" "$rc"

o="$T/o8/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run "$good" windows 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "a target with no prebuilt daemon is refused" fail "Linux and darwin/arm64 only" "$out" "$rc"

mac="$T/mac"; darwin_fixture "$mac" 0.7.0 abc1234 2026-09-24T00:00:00Z
o="$T/o9/mgit-sandboxd-darwin_darwin_arm64_v8.0/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run_darwin "$mac" arm64 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "darwin/arm64: the daemon, lib/ and THIRD_PARTY/ are placed" ok "manifest verified in and out" "$out" "$rc"
[ -f "$(dirname "$o")/lib/libkrun.1.dylib" ] && [ -f "$(dirname "$o")/THIRD_PARTY/libkrun-LICENSE" ] ||
	{ echo "  FAIL  the darwin case did not place lib/ and THIRD_PARTY/"; failures=$((failures + 1)); }

o="$T/o10/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run_darwin "$mac" arm64 0.7.0 def5678 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "darwin: a release whose commit differs from the daemon's is refused" fail "stamped commit=abc1234" "$out" "$rc"

o="$T/o11/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run_darwin "$mac" amd64 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "darwin/amd64 has no prebuilt daemon and is refused" fail "Linux and darwin/arm64 only" "$out" "$rc"

out="$(env -u MGIT_DARWIN_PREBUILT GOOS=darwin GOARCH=arm64 bash "$shim" build -o "$T/o12/mgit-sandboxd" . 2>&1)"; rc=$?
expect "MGIT_DARWIN_PREBUILT unset: refused with the build instructions" fail "build-darwin-sandboxd.sh" "$out" "$rc"

o="$T/o13/x/mgit-sandboxd"; mkdir -p "$(dirname "$o")"
out="$(run_darwin "$good" arm64 0.7.0 abc1234 2026-09-24T00:00:00Z "$o")"; rc=$?
expect "darwin: a root holding only Linux prebuilts is refused" fail "no prebuilt daemon at" "$out" "$rc"

out="$(bash "$shim" version 2>&1)"; rc=$?
expect "any verb but build is refused" fail "only answers 'build'" "$out" "$rc"

if [ "$failures" -ne 0 ]; then
	echo "gobinary-prebuilt selftest: $failures case(s) FAILED"
	exit 1
fi
echo "gobinary-prebuilt selftest: PASS"
