#!/usr/bin/env bash
# guest-tools-urls-selftest.sh — fetch-guest-tools.sh names, for each arch, the
# exact publisher URL of every pinned archive, built from the release and asset
# names in guest-tools.env (never from a semver that 404s). Refs: FEAT-6.104
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }

echo "== guest tools URLs self-test =="
for arch in amd64 arm64; do
  out="$(bash "$here/fetch-guest-tools.sh" --print-urls "$arch" 2>&1)" || { bad "$arch: --print-urls failed: $out"; continue; }
  n="$(printf '%s\n' "$out" | grep -c ' https://')"
  [ "$n" -eq 5 ] && ok "$arch: five archive URLs" || bad "$arch: expected 5 URLs, got $n"
  # one line per URL: "<tool> <sha256> <url>"
  printf '%s\n' "$out" | grep -q "^jre .*jdk-21.0.12.1%2B1/OpenJDK21U-jre_.*_linux_hotspot_21.0.12.1_1.tar.gz$" \
    && ok "$arch: JRE URL is built from the release name" || bad "$arch: JRE URL is not built from the release name"
  printf '%s\n' "$out" | grep -q "^node .*nodejs.org/dist/v22.23.3/node-v22.23.3-linux-.*\.tar\.xz$" \
    && ok "$arch: Node URL names the .tar.xz asset" || bad "$arch: Node URL does not name the .tar.xz asset"
  printf '%s\n' "$out" | grep -q "^go .*go.dev/dl/go1.26.10.linux-$arch.tar.gz$" \
    && ok "$arch: Go URL" || bad "$arch: Go URL wrong"
  printf '%s\n' "$out" | grep -q "^tlc .*tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar$" \
    && ok "$arch: TLC URL" || bad "$arch: TLC URL wrong"
  printf '%s\n' "$out" | grep -q "^golangci-lint .*golangci-lint-2.11.4-linux-$arch.tar.gz$" \
    && ok "$arch: golangci-lint URL" || bad "$arch: golangci-lint URL wrong"
done
[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
