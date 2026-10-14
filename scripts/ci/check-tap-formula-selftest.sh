#!/usr/bin/env bash
# check-tap-formula-selftest.sh — prove check-tap-formula.sh passes when the
# tap's formula body matches brew/mgit.rb, FAILS with a readable diff when it
# drifts, SKIPS loudly (and exits 0) when the tap cannot be fetched, and refuses
# to skip under --require. Offline: every "tap" below is a file:// fixture.
# Refs: MGIT-67
# fetch-guard-file: every fetch here is a file:// URL to a fixture under mktemp
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
CHECK="$HERE/check-tap-formula.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/tapchk.XXXXXX")" || { echo "no scratch root" >&2; exit 99; }
trap 'find "$WORK" -type f -delete; find "$WORK" -depth -type d -empty -delete' EXIT
fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }

echo "== tap formula drift self-test =="
[ -f "$CHECK" ] || { bad "check-tap-formula.sh does not exist"; echo "$fails failure(s)" >&2; exit 1; }

SRC="$REPO/brew/mgit.rb"
# the generated lines, as the tap's update-formula.yml writes them
sed -e 's/^\( *version "\)[^"]*"/\10.9.9"/' -e 's/^\( *sha256 "\)[0-9a-f]*"/\1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/' "$SRC" > "$WORK/synced.rb"

run() { # run <tap file or missing path> [extra args]; sets out and rc
	local tap="$1"; shift
	out="$(bash "$CHECK" --repo-formula "$SRC" --tap-url "file://$tap" "$@" 2>&1)"; rc=$?
}

run "$WORK/synced.rb"
[ "$rc" -eq 0 ] && ok "a tap body that differs only in version and checksums is in sync" || bad "in-sync tap flagged (rc=$rc): $out"
cmp -s "$WORK/synced.rb" "$SRC" && bad "fixture is identical to the source, so the ignore rule is untested"

# drift: the install shape changed in the repo but not in the tap
python3 - "$WORK/synced.rb" "$WORK/drift.rb" <<'PY'
import sys
s = open(sys.argv[1]).read()
i = s.index("def install")
j = s.index("\n", i)
open(sys.argv[2], "w").write(s[:j + 1] + "    # drifted: the tap body lost a line the repo has\n" + s[j + 1:])
PY
run "$WORK/drift.rb"
[ "$rc" -eq 1 ] && ok "a drifted body fails" || bad "drift not detected (rc=$rc): $out"
printf '%s\n' "$out" | grep -q 'drifted: the tap body lost a line' && ok "the failure shows the differing line" || bad "no diff in the failure output: $out"
printf '%s\n' "$out" | grep -qi 'copy' && ok "the failure says how to resync" || bad "no remedy named: $out"

# a changed checksum alone is not drift
sed 's/aaaaaaaa/bbbbbbbb/' "$WORK/synced.rb" > "$WORK/sums.rb"
run "$WORK/sums.rb"
[ "$rc" -eq 0 ] && ok "changed checksums alone are not drift" || bad "checksum-only change flagged: $out"

# an unfetchable tap skips loudly, and only --require makes it fatal
run "$WORK/does-not-exist.rb"
[ "$rc" -eq 0 ] && printf '%s\n' "$out" | grep -q 'SKIPPED' && ok "an unfetchable tap SKIPS loudly with exit 0" || bad "unfetchable tap: rc=$rc out=$out"
printf '%s\n' "$out" | grep -qi 'not checked' && ok "the skip says the formula was not checked" || bad "skip does not say NOT CHECKED: $out"
run "$WORK/does-not-exist.rb" --require
[ "$rc" -eq 3 ] && ok "--require turns a skip into exit 3" || bad "--require did not fail the skip (rc=$rc)"

# an empty fetch is a failed fetch, not an empty formula
: > "$WORK/empty.rb"
run "$WORK/empty.rb"
printf '%s\n' "$out" | grep -q 'SKIPPED' && ok "an empty tap file is a skip, not a pass" || bad "empty tap read as in sync: rc=$rc $out"

[ "$fails" -eq 0 ] && { echo "all checks passed"; exit 0; }
echo "$fails failure(s)" >&2
exit 1
