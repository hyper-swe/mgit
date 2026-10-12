#!/usr/bin/env bash
# guest-tools-selftest.sh — the verification-tools manifest for the guest base
# pins every tool the base must carry, each by version and sha256, and the Go
# pin matches the repository's own go.mod. Refs: FEAT-6.104
#
# Usage: bash scripts/sandbox-image/guest-tools-selftest.sh
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
manifest="$here/guest-tools.env"

ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }
fails=0

echo "== guest tools manifest self-test =="
if [ ! -f "$manifest" ]; then
  bad "guest-tools.env is missing"
  echo "$fails failure(s)" >&2
  exit 1
fi
# shellcheck source=scripts/sandbox-image/guest-tools.env
. "$manifest"

need() {
  local name="$1" val
  val="${!name:-}"
  [ -n "$val" ] && ok "$name is set" || bad "$name is not set"
}
sha() {
  local name="$1" val
  val="${!name:-}"
  [[ "$val" =~ ^[0-9a-f]{64}$ ]] && ok "$name is a sha256" || bad "$name is not a 64-hex sha256 (got '${val}')"
}

for t in GO JRE TLC GOLANGCI_LINT GOVULNCHECK NODE; do
  need "${t}_VERSION"
  sha "${t}_SHA256_AMD64"
  sha "${t}_SHA256_ARM64"
done

gomod="$(awk '/^go /{print $2; exit}' "$root/go.mod")"
[ "${GO_VERSION:-}" = "$gomod" ] && ok "GO_VERSION matches go.mod ($gomod)" || bad "GO_VERSION '${GO_VERSION:-}' != go.mod '$gomod'"
[ "${GOLANGCI_LINT_VERSION:-}" = "2.11.4" ] && ok "golangci-lint pinned 2.11.4" || bad "golangci-lint is not 2.11.4"
[ "${GOVULNCHECK_VERSION:-}" = "1.8.0" ] && ok "govulncheck pinned 1.8.0" || bad "govulncheck is not 1.8.0"
case "${NODE_VERSION:-}" in 22.*) ok "Node is 22.x";; *) bad "Node is not 22.x";; esac
case "${JRE_VERSION:-}" in 17|17.*|2[1-9]|2[1-9].*) ok "JRE >= 17";; *) bad "JRE is not >= 17 (got '${JRE_VERSION:-}')";; esac

[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
