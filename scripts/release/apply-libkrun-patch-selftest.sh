#!/usr/bin/env bash
# Self-test for scripts/release/apply-libkrun-patch.sh against fixture trees.
# Refs: MGIT-259
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
helper="$here/apply-libkrun-patch.sh"
T="$(mktemp -d "${TMPDIR:-/tmp}/apply-libkrun-patch-selftest.XXXXXX")"
case "$T" in "${TMPDIR:-/tmp}"/apply-libkrun-patch-selftest.*) ;; *) echo "refusing: unexpected scratch dir $T" >&2; exit 2 ;; esac
trap 'rm -rf "$T"' EXIT
failures=0

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
# tree <dir>: a fixture work tree holding one committed file.
tree() {
	case "$1" in "$T"/*) ;; *) echo "refusing: fixture $1 is outside $T" >&2; exit 2 ;; esac
	mkdir -p "$1"
	printf 'one\ntwo\nthree\n' >"$1/a.txt"
	git -C "$1" init -q
	git -C "$1" -c user.name=selftest -c user.email=selftest@invalid add a.txt
	git -C "$1" -c user.name=selftest -c user.email=selftest@invalid commit -qm fixture
}
expect() { # <label> <want: ok|fail> <needle> <output> <rc>
	local label="$1" want="$2" needle="$3" out="$4" rc="$5"
	if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = fail ] && [ "$rc" -ne 0 ]; }; then
		case "$out" in *"$needle"*) echo "  pass  $label"; return ;; esac
	fi
	echo "  FAIL  $label (rc=$rc, wanted $want with \"$needle\"): $out"
	failures=$((failures + 1))
}
unchanged() { # <label> <tree>
	[ "$(cat "$2/a.txt")" = "$(printf 'one\ntwo\nthree')" ] ||
		{ echo "  FAIL  $1: the refused case changed the tree"; failures=$((failures + 1)); }
}

patch="$T/fix.patch"
{
	echo "MGIT-225"
	printf 'diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n'
} >"$patch"
pin="$(sha256 "$patch")"

tree "$T/good"
out="$(bash "$helper" "$T/good" "$patch" "$pin" 2>&1)"; rc=$?
expect "the pinned patch applies to a clean tree" ok "applied to" "$out" "$rc"
grep -qx TWO "$T/good/a.txt" || { echo "  FAIL  the positive case did not change the tree"; failures=$((failures + 1)); }

out="$(bash "$helper" "$T/good" "$patch" "$pin" 2>&1)"; rc=$?
expect "a tree that already carries the patch is refused" fail "does not apply cleanly" "$out" "$rc"

tree "$T/digest"
out="$(bash "$helper" "$T/digest" "$patch" "$(printf '0%.0s' $(seq 64))" 2>&1)"; rc=$?
expect "a patch whose digest is not the pin is refused" fail "not the pinned" "$out" "$rc"
unchanged "digest" "$T/digest"

tree "$T/moved"
printf 'one\nzwei\nthree\n' >"$T/moved/a.txt"
out="$(bash "$helper" "$T/moved" "$patch" "$pin" 2>&1)"; rc=$?
expect "a patch that does not apply cleanly is refused" fail "does not apply cleanly" "$out" "$rc"
grep -qx zwei "$T/moved/a.txt" || { echo "  FAIL  the refused case changed the moved tree"; failures=$((failures + 1)); }

mkdir -p "$T/plain"
out="$(bash "$helper" "$T/plain" "$patch" "$pin" 2>&1)"; rc=$?
expect "a directory that is not a git work tree is refused" fail "not a git work tree" "$out" "$rc"

tree "$T/outer"
mkdir -p "$T/outer/sub"
out="$(bash "$helper" "$T/outer/sub" "$patch" "$pin" 2>&1)"; rc=$?
expect "a subdirectory of another work tree is refused" fail "not the top of its work tree" "$out" "$rc"
unchanged "outer" "$T/outer"

tree "$T/nopatch"
out="$(bash "$helper" "$T/nopatch" "$T/absent.patch" "$pin" 2>&1)"; rc=$?
expect "a missing patch is refused" fail "no patch at" "$out" "$rc"
unchanged "nopatch" "$T/nopatch"

out="$(bash "$helper" "$T/good" "$patch" 2>&1)"; rc=$?
expect "a call without the digest is refused" fail "usage:" "$out" "$rc"

if [ "$failures" -ne 0 ]; then
	echo "apply-libkrun-patch selftest: $failures case(s) FAILED"
	exit 1
fi
echo "apply-libkrun-patch selftest: PASS"
