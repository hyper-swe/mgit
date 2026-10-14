#!/usr/bin/env bash
# guest-debs-check-selftest.sh — the pinned .deb lock is checked against the
# security archive and against the base image, on small fixtures. Refs: FEAT-6.104.1
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
fx="$here/testdata/debs"
chk="$here/guest-debs-check.py"
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }

echo "== guest debs check self-test =="
out="$(python3 "$chk" security --lock "$fx/lock-old.lock" --index "$fx/security-newer.index" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && ok "a newer security version fails" || bad "a newer security version passed"
printf '%s\n' "$out" | grep -q 'libssl3.*3.0.20-1~deb12u2.*3.0.21-1~deb12u1' && ok "the failure names the package and both versions" || bad "the failure does not name libssl3 and both versions: $out"
python3 "$chk" security --lock "$fx/lock-old.lock" --index "$fx/security-same.index" >/dev/null 2>&1 && ok "the same security version passes" || bad "the same security version failed"

out="$(python3 "$chk" base --requires "$fx/requires-stale.txt" --status "$fx/base-status" 2>&1)"; rc=$?
[ "$rc" -ne 0 ] && ok "a perl-base that differs from the base fails" || bad "a stale perl-base passed"
printf '%s\n' "$out" | grep -q 'perl-base.*5.36.0-7+deb12u2.*5.36.0-7+deb12u3' && ok "the failure names perl-base and both versions" || bad "the failure does not name perl-base and both versions: $out"
python3 "$chk" base --requires "$fx/requires-match.txt" --status "$fx/base-status" >/dev/null 2>&1 && ok "a matching perl-base passes" || bad "a matching perl-base failed"

# Debian version order: tilde sorts before anything, an epoch beats the rest
python3 - "$chk" <<'PY' && ok "version comparison follows Debian order" || bad "version comparison is wrong"
import importlib.util, sys
spec = importlib.util.spec_from_file_location("chk", sys.argv[1]); m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
c = m.compare
assert c("1.0~rc1", "1.0") < 0 and c("1.0", "1.0~rc1") > 0
assert c("1:0.9", "2.0") > 0 and c("1.0-1", "1.0-1") == 0
assert c("3.0.21-1~deb12u1", "3.0.20-1~deb12u2") > 0 and c("1.10", "1.9") > 0
assert c("5.36.0-7+deb12u3", "5.36.0-7+deb12u2") > 0 and c("2.36-9+deb12u13", "2.36-9+deb12u9") > 0
PY
for arch in amd64 arm64; do
  [ -s "$here/guest-debs-$arch.requires" ] && grep -q '^perl-base ' "$here/guest-debs-$arch.requires" \
    && ok "$arch: the committed requires file pins perl-base" || bad "$arch: no committed requires file with perl-base"
done
[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
