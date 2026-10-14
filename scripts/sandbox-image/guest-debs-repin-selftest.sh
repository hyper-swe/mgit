#!/usr/bin/env bash
# guest-debs-repin-selftest.sh — repin-guest-debs.sh re-pins the .deb lock against
# bookworm AND bookworm-security snapshots, verifies the signed Release file and
# every index hash first, and fails closed. Runs offline against a fixture mirror.
# Refs: FEAT-6.104.1
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }

W="$(mktemp -d "${TMPDIR:-/tmp}/repin-selftest.XXXXXX")" || { echo "mktemp failed" >&2; exit 1; }
cleanup() { find "$W" -type f -delete; find "$W" -depth -type d -empty -delete; }
trap cleanup EXIT

MAIN_TS=20261020T000000Z
SEC_TS=20261021T000000Z
M="$W/mirror"
mkdir -p "$M/archive/debian/$MAIN_TS/dists/bookworm/main/binary-arm64" \
         "$M/archive/debian-security/$SEC_TS/dists/bookworm-security/main/binary-arm64" "$W/bin"

sha_of() { shasum -a 256 "$1" | cut -c1-64; }
# publish <suite dir> <Packages text file>: write Packages.xz and a Release listing it
publish() {
  local dir="$1" src="$2" suite="$3" sub="$4"
  cp "$src" "$dir/$sub/Packages"; xz -k -f "$dir/$sub/Packages"; rm -- "$dir/$sub/Packages"
  local f="$dir/$sub/Packages.xz"
  { echo "Suite: $suite"; echo "SHA256:"; echo " $(sha_of "$f") $(wc -c < "$f" | tr -d ' ') $sub/Packages.xz"; } > "$dir/InRelease"
}

cat > "$W/main.idx" <<'IDX'
Package: make
Version: 4.3-4.1
Architecture: arm64
Depends: libc6 (>= 2.34)
Filename: pool/main/m/make-dfsg/make_4.3-4.1_arm64.deb
Size: 100
SHA256: 1111111111111111111111111111111111111111111111111111111111111111

Package: perl
Version: 5.36.0-7+deb12u3
Architecture: arm64
Depends: perl-base (= 5.36.0-7+deb12u3), libssl3 (>= 3.0)
Filename: pool/main/p/perl/perl_5.36.0-7+deb12u3_arm64.deb
Size: 200
SHA256: 2222222222222222222222222222222222222222222222222222222222222222

Package: libssl3
Version: 3.0.20-1~deb12u2
Architecture: arm64
Filename: pool/main/o/openssl/libssl3_3.0.20-1~deb12u2_arm64.deb
Size: 300
SHA256: 3333333333333333333333333333333333333333333333333333333333333333
IDX
cat > "$W/sec.idx" <<'IDX'
Package: libssl3
Version: 3.0.21-1~deb12u1
Architecture: arm64
Filename: pool/updates/main/o/openssl/libssl3_3.0.21-1~deb12u1_arm64.deb
Size: 310
SHA256: 4444444444444444444444444444444444444444444444444444444444444444
IDX
cat > "$W/status" <<'ST'
Package: libc6
Status: install ok installed
Version: 2.36-9+deb12u13
Architecture: arm64

Package: perl-base
Status: install ok installed
Version: 5.36.0-7+deb12u3
Architecture: arm64
ST
echo fake-keyring > "$W/keyring.gpg"
publish "$M/archive/debian/$MAIN_TS/dists/bookworm" "$W/main.idx" bookworm main/binary-arm64
publish "$M/archive/debian-security/$SEC_TS/dists/bookworm-security" "$W/sec.idx" bookworm-security main/binary-arm64

printf '#!/bin/sh\nexit 0\n' > "$W/bin/gpgv-ok"; printf '#!/bin/sh\necho BAD signature >&2\nexit 1\n' > "$W/bin/gpgv-bad"
chmod +x "$W/bin/gpgv-ok" "$W/bin/gpgv-bad"

run() { # run <gpgv command> <out dir>
  GPGV="$1" bash "$here/repin-guest-debs.sh" --arch arm64 --base-url "file://$M" \
    --main-snapshot "$MAIN_TS" --security-snapshot "$SEC_TS" --base-status "$W/status" \
    --keyring "$W/keyring.gpg" --want "make perl" --out-dir "$2"
}

echo "== guest debs re-pin self-test =="
out="$(run "$W/bin/gpgv-ok" "$W/out1" 2>&1)" || bad "re-pin failed on a good mirror: $out"
lock="$W/out1/guest-debs-arm64.lock"
if [ -f "$lock" ]; then
  grep -q '^libssl3 3.0.21-1~deb12u1 4444' "$lock" && ok "the newer security libssl3 is pinned over main's" || bad "libssl3 is not the security version"
  grep -q "^libssl3 .* file://$M/archive/debian-security/$SEC_TS/pool/updates/main/o/openssl/" "$lock" && ok "the security package's URL is under the security snapshot" || bad "wrong URL for the security package: $(grep '^libssl3' "$lock")"
  grep -q "^make 4.3-4.1 1111.* file://$M/archive/debian/$MAIN_TS/pool/main/m/" "$lock" && ok "a main package's URL is under the main snapshot" || bad "wrong URL for make"
  grep -q "$MAIN_TS" "$lock" && grep -q "$SEC_TS" "$lock" && ok "the lock header records both snapshot timestamps" || bad "snapshot timestamps not recorded"
  grep -q '^perl-base 5.36.0-7+deb12u3$' "$W/out1/guest-debs-arm64.requires" && ok "the requires file pins perl-base" || bad "no perl-base requirement"
  python3 "$here/guest-debs-check.py" security --lock "$lock" --index "$W/sec.idx" >/dev/null 2>&1 && ok "the new lock passes the security check" || bad "the new lock fails the security check"
else
  bad "no lock was written"
fi

out="$(run "$W/bin/gpgv-bad" "$W/out2" 2>&1)" && bad "a bad signature passed" || ok "a bad signature fails"
[ ! -e "$W/out2/guest-debs-arm64.lock" ] && ok "a bad signature writes no lock" || bad "a lock was written after a bad signature"

out="$(run "$W/no-such-gpgv" "$W/out3" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && printf '%s\n' "$out" | grep -q 'NOT CHECKED' && ok "a missing gpgv fails and says NOT CHECKED" || bad "a missing gpgv did not fail with NOT CHECKED: $out"

printf 'tampered' >> "$M/archive/debian-security/$SEC_TS/dists/bookworm-security/main/binary-arm64/Packages.xz"
out="$(run "$W/bin/gpgv-ok" "$W/out4" 2>&1)" && bad "a tampered index passed" || ok "a tampered index fails the hash check"
[ ! -e "$W/out4/guest-debs-arm64.lock" ] && ok "a tampered index writes no lock" || bad "a lock was written after a tampered index"

[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
