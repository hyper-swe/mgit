#!/usr/bin/env bash
# Carry a patched libkrun in the macOS build; fixes MGIT-225.
#
# Usage: apply-libkrun-patch.sh <libkrun-tree> <patch> <sha256>
# Refs: MGIT-259
set -euo pipefail

say() { echo "apply-libkrun-patch: $*" >&2; }
die() { say "FATAL: $*"; exit 1; }

[ "$#" -eq 3 ] || die "usage: apply-libkrun-patch.sh <libkrun-tree> <patch> <sha256>"
tree="$1" patch="$2" want="$3"

[ -f "$patch" ] || die "no patch at $patch"
git -C "$tree" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "$tree is not a git work tree"
top="$(git -C "$tree" rev-parse --show-toplevel)"
[ "$top" -ef "$tree" ] || die "$tree is not the top of its work tree ($top is)"

if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$patch" | cut -d' ' -f1)"
else
	got="$(shasum -a 256 "$patch" | cut -d' ' -f1)"
fi
[ "$got" = "$want" ] || die "$patch has digest $got, not the pinned $want"

patch="$(cd "$(dirname "$patch")" && pwd)/$(basename "$patch")"
git -C "$tree" apply --check "$patch" 2>/dev/null || die "$patch does not apply cleanly to $tree"
git -C "$tree" apply "$patch"
git -C "$tree" apply --reverse --check "$patch" 2>/dev/null || die "$patch did not apply to $tree"
say "$(basename "$patch") applied to $tree (sha256 $got)"
