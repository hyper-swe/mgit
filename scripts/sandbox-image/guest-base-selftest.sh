#!/usr/bin/env bash
# guest-base-selftest.sh — the glibc guest base plan: built on the Debian slim
# image pinned by digest in pins.env, carries every pinned verification tool,
# is fetched fail-closed under the fetch guard, and stays under the size cap.
# Refs: FEAT-6.104
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }
# shellcheck source=scripts/sandbox-image/pins.env
. "$here/pins.env"

echo "== guest base plan self-test =="
for arch in amd64 arm64; do
  out="$(bash "$here/build-guest-base.sh" --print-plan "$arch" 2>&1)" || { bad "$arch: --print-plan failed: $out"; continue; }
  want="BUILDER_$(echo "$arch" | tr a-z A-Z)"
  digest="${!want#*@}"
  printf '%s\n' "$out" | grep -q "^base $digest$" && ok "$arch: base is the pinned digest" || bad "$arch: base is not $digest"
  for t in go jre tlc golangci-lint node; do
    printf '%s\n' "$out" | grep -q "^tool $t " && ok "$arch: carries $t" || bad "$arch: missing $t"
  done
  # git and make come from pinned .deb files (the slim base has neither and the
  # guest has no network): "deb <package> <version> <sha256> <snapshot url>".
  for p in git make; do
    printf '%s\n' "$out" | grep -Eq "^deb $p [^ ]+ [0-9a-f]{64} https://snapshot.debian.org/archive/debian/[0-9TZ]+/pool/.*\.deb$" \
      && ok "$arch: $p is a pinned .deb" || bad "$arch: $p is not a pinned .deb"
  done
  nd="$(printf '%s\n' "$out" | grep -c '^deb ')"
  [ "$nd" -ge 20 ] && ok "$arch: $nd pinned .deb files" || bad "$arch: only $nd pinned .deb files"
  printf '%s\n' "$out" | grep -q '^fetch guard-fetch.sh .*-c ' && ok "$arch: fetched under the guard" || bad "$arch: not under the guard"
  printf '%s\n' "$out" | grep -q '^max-mib 1229$' && ok "$arch: size cap 1229 MiB (1.2 GiB)" || bad "$arch: no 1229 MiB cap"
  printf '%s\n' "$out" | grep -q '^network none$' && ok "$arch: no run-time network" || bad "$arch: network not none"
done
[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
