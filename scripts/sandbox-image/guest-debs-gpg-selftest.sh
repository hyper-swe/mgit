#!/usr/bin/env bash
# Real-signature self-test for verify-release.sh: a throwaway key signs a fixture InRelease and the
# real gpgv checks it. Cases: good signature; tampered signed body; signature by a different key;
# an unsigned hash block placed above a validly signed Release.
# Needs gpg and gpgv. With --require a missing tool exits 3 (CI); without it the script says
# NOT CHECKED and exits 0 so a host without gnupg is never read as a pass.
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
if ! command -v gpg >/dev/null 2>&1 || ! command -v gpgv >/dev/null 2>&1; then
  echo "guest-debs-gpg-selftest: NOT CHECKED: gpg or gpgv is not installed" >&2
  [ "${1:-}" = "--require" ] && exit 3
  exit 0
fi
W="$(mktemp -d "${TMPDIR:-/tmp}/gpgst.XXXXXX")" || { echo "mktemp failed" >&2; exit 1; }
cleanup() { gpgconf --kill gpg-agent >/dev/null 2>&1; find "$W" -type f -delete; find "$W" -depth -type d -empty -delete; }
trap cleanup EXIT
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }
export GNUPGHOME="$W/home"; mkdir -m 700 "$GNUPGHOME"
mkkey() { gpg --batch --quiet --pinentry-mode loopback --passphrase '' --quick-generate-key "$1 <$1@example.org>" ed25519 sign never; }
mkkey good; mkkey other
gpg --export good@example.org > "$W/keyring.gpg"

printf 'fake packages' > "$W/Packages.xz"
sum="$(shasum -a 256 "$W/Packages.xz" | cut -c1-64)"
printf 'Suite: bookworm\nSHA256:\n %s 13 main/binary-arm64/Packages.xz\n' "$sum" > "$W/Release"
sign() { gpg --batch --quiet --pinentry-mode loopback --passphrase '' --local-user "$1" --clearsign --output "$2" "$W/Release"; }
check() { mkdir -p "$W/w"; bash "$here/verify-release.sh" "$W/keyring.gpg" "$1" bookworm main/binary-arm64/Packages.xz "$W/Packages.xz" "$W/w" t; }

echo "== real gpgv signature self-test =="
sign good@example.org "$W/good.InRelease"
check "$W/good.InRelease" && ok "a good signature passes" || bad "a good signature failed"

# the signed body is edited to list a forged file's hash, so only the signature can refuse it
fsum="$(printf 'forged' | shasum -a 256 | cut -c1-64)"
printf 'forged' > "$W/Packages.xz"
sed "s/$sum 13/$fsum 6/" "$W/good.InRelease" > "$W/tampered.InRelease"
check "$W/tampered.InRelease" >/dev/null 2>&1 && bad "a tampered signed body passed" || ok "a tampered signed body fails"

sign other@example.org "$W/other.InRelease"
printf 'fake packages' > "$W/Packages.xz"   # the genuine file again: only the key can refuse this one
check "$W/other.InRelease" >/dev/null 2>&1 && bad "a signature by another key passed" || ok "a signature by another key fails"

{ printf 'SHA256:\n %s 6 main/binary-arm64/Packages.xz\n' "$fsum"; cat "$W/good.InRelease"; } > "$W/prefixed.InRelease"
check "$W/prefixed.InRelease" >/dev/null 2>&1 && bad "an unsigned block above a signed Release passed" || ok "an unsigned block above a signed Release fails"

[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
