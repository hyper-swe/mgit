#!/usr/bin/env bash
# fetch-guest-tools.sh — fetch the pinned verification-tool archives for one
# architecture, each digest-verified, into a directory. Refs: FEAT-6.104
#
# Usage: fetch-guest-tools.sh --print-urls <amd64|arm64>
#        fetch-guest-tools.sh <amd64|arm64> <dest-dir>
# --print-urls prints "<tool> <sha256> <url>" per archive and fetches nothing.
# A fetch is the only network step; the guest itself runs with no network.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=guest-tools.env
. "$HERE/guest-tools.env"

print=0
if [ "${1:-}" = "--print-urls" ]; then print=1; shift; fi
ARCH="${1:?usage: fetch-guest-tools.sh [--print-urls] <amd64|arm64> [dest-dir]}"
case "$ARCH" in
	amd64) U=AMD64 ;;
	arm64) U=ARM64 ;;
	*) echo "fetch-guest-tools: unknown arch '$ARCH'" >&2; exit 2 ;;
esac
pick() { local n="$1_$U"; printf '%s' "${!n}"; }

jre_release_url="${JRE_RELEASE//+/%2B}"
list() {
	echo "go $(pick GO_SHA256) https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz"
	echo "jre $(pick JRE_SHA256) https://github.com/adoptium/temurin21-binaries/releases/download/${jre_release_url}/$(pick JRE_ASSET)"
	echo "tlc ${TLC_SHA256} https://github.com/tlaplus/tlaplus/releases/download/v${TLC_VERSION}/tla2tools.jar"
	echo "golangci-lint $(pick GOLANGCI_LINT_SHA256) https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${ARCH}.tar.gz"
	echo "node $(pick NODE_SHA256) https://nodejs.org/dist/v${NODE_VERSION}/$(pick NODE_ASSET)"
}

if [ "$print" -eq 1 ]; then list; exit 0; fi

DEST="${2:?usage: fetch-guest-tools.sh <amd64|arm64> <dest-dir>}"
mkdir -p "$DEST"
# shellcheck source=prefetch.sh
. "$HERE/prefetch.sh"
list | while read -r tool sum url; do
	prefetch_verified "$url" "$sum" "$DEST/$(basename "$url")" || { echo "fetch-guest-tools: $tool failed" >&2; exit 1; }
done
